// Package manifest decides which files of a working directory go to a Scratch
// machine, and describes them.
//
// Three rules shape it, each from the design (section 5 and 6):
//
//  1. The laptop is the only source of data. Nothing that looks like a
//     credential leaves it: a secret-shaped file is skipped, named in the
//     output, and never read, so not even a hash of it is computed. An
//     explicit per-file allow overrides that one file and no other.
//  2. Nothing outside the working directory is sent. A symlink is skipped
//     (its target could be anywhere), a path that escapes the root is refused,
//     and the version-control directory is never sent.
//  3. A workspace over the limit is refused before any content is read.
//
// The manifest is a description, not a transfer: it holds paths, sizes, mode
// bits and SHA-256 digests. The transport moves the bytes.
package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"

	"github.com/runners-io/cli/internal/platform"
)

// DefaultMaxWorkspaceBytes is the workspace limit, 2 GiB. It is an ASSUMED
// starting point (design section 6, marked [A]) to be measured.
const DefaultMaxWorkspaceBytes int64 = 2 << 30

// Why a candidate was not sent.
const (
	ReasonSecret     = "secret"
	ReasonSymlink    = "symlink"
	ReasonGit        = "git_dir"
	ReasonMissing    = "missing"
	ReasonNotRegular = "not_regular"
)

// Options tune Build.
type Options struct {
	// MaxWorkspaceBytes is the largest total size of the files to send. Zero
	// means DefaultMaxWorkspaceBytes.
	MaxWorkspaceBytes int64
	// Allow names secret-shaped files to send anyway, by exact path.
	Allow []string
}

// Entry is one file to send.
type Entry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Executable bool   `json:"executable"`
	SHA256     string `json:"sha256"`
}

// Skip is one candidate that is not sent, and why.
type Skip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Manifest is the description of a sync.
type Manifest struct {
	Entries    []Entry
	Skipped    []Skip
	TotalBytes int64
	// UnusedAllow lists Allow paths that matched no candidate, so a typo is
	// visible instead of silently leaving a file behind.
	UnusedAllow []string
}

// Digest identifies the set of files and their contents and modes. Skipped
// files are not part of it.
func (m Manifest) Digest() string {
	h := sha256.New()
	for _, e := range m.Entries {
		// One JSON object per line: a path may hold any character a filename
		// can, and JSON escapes the ones that would blur the framing.
		b, _ := json.Marshal(e)
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IsSecretPath reports whether a slash-separated relative path looks like a
// credential, and which rule said so. The list is the design's (.env*, *.pem,
// *.key, .aws/) plus the other credential shapes this repository's own secret
// scanner already refuses. It matches on the NAME only; it is a guard against
// accidents, not a scanner, and says so: a secret inside an ordinary file is
// not found.
func IsSecretPath(p string) (bool, string) {
	parts := strings.Split(p, "/")
	for _, dir := range parts[:len(parts)-1] {
		switch strings.ToLower(dir) {
		case ".aws", ".ssh", ".gnupg":
			return true, "credential directory " + dir
		}
	}
	base := strings.ToLower(parts[len(parts)-1])
	switch {
	case strings.HasPrefix(base, ".env"):
		return true, "environment file"
	case base == ".netrc", base == ".git-credentials":
		return true, "credential file"
	case base == "id_rsa", base == "id_dsa", base == "id_ecdsa", base == "id_ed25519":
		return true, "ssh private key"
	case strings.HasSuffix(base, ".tfstate"), strings.Contains(base, ".tfstate."):
		return true, "terraform state"
	}
	switch path.Ext(base) {
	case ".pem", ".key", ".p12", ".pfx":
		return true, "key or certificate file"
	}
	return false, ""
}

func hasGitComponent(p string) bool {
	for _, c := range strings.Split(p, "/") {
		if c == ".git" {
			return true
		}
	}
	return false
}

func validCandidate(p string) bool {
	return p != "." && fs.ValidPath(p) && !strings.Contains(p, `\`)
}

// Build describes the candidates found under fsys. The candidates come from a
// lister (ListGit or ListWalk); Build does not trust them: each is validated,
// classified and re-stat'ed.
func Build(fsys fs.FS, candidates []string, opts Options) (Manifest, error) {
	limit := opts.MaxWorkspaceBytes
	if limit == 0 {
		limit = DefaultMaxWorkspaceBytes
	}
	allow := map[string]bool{}
	for _, a := range opts.Allow {
		allow[a] = false
	}

	seen := map[string]bool{}
	var m Manifest
	type pending struct {
		path string
		info fs.FileInfo
	}
	var todo []pending

	// Pass 1: classify and stat. No content is read here.
	for _, c := range candidates {
		if !validCandidate(c) {
			return Manifest{}, platform.Errorf(platform.KindUsage, "refusing path %q: it is not a relative path inside the working directory", c)
		}
		if seen[c] {
			continue
		}
		seen[c] = true

		if hasGitComponent(c) {
			m.Skipped = append(m.Skipped, Skip{c, ReasonGit})
			continue
		}
		if secret, _ := IsSecretPath(c); secret {
			if _, ok := allow[c]; ok {
				allow[c] = true
			} else {
				m.Skipped = append(m.Skipped, Skip{c, ReasonSecret})
				continue
			}
		}
		info, err := fs.Lstat(fsys, c)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			m.Skipped = append(m.Skipped, Skip{c, ReasonMissing})
			continue
		case err != nil:
			return Manifest{}, fmt.Errorf("stat %s: %w", c, err)
		case info.Mode()&fs.ModeSymlink != 0:
			m.Skipped = append(m.Skipped, Skip{c, ReasonSymlink})
			continue
		case !info.Mode().IsRegular():
			m.Skipped = append(m.Skipped, Skip{c, ReasonNotRegular})
			continue
		}
		m.TotalBytes += info.Size()
		todo = append(todo, pending{c, info})
	}

	for a, used := range allow {
		if !used {
			// An allow for a file that is not secret-shaped is also "unused":
			// it did nothing, and the user should know.
			m.UnusedAllow = append(m.UnusedAllow, a)
		}
	}
	sort.Strings(m.UnusedAllow)

	if m.TotalBytes > limit {
		return Manifest{}, platform.Errorf(platform.KindSyncTooLarge,
			"the workspace is %s in %d files, over the %s limit; narrow it with a smaller --dir or remove build output",
			byteSize(m.TotalBytes), len(todo), byteSize(limit))
	}

	// Pass 2: hash.
	sort.Slice(todo, func(i, j int) bool { return todo[i].path < todo[j].path })
	for _, p := range todo {
		sum, err := hashFile(fsys, p.path)
		if err != nil {
			return Manifest{}, fmt.Errorf("read %s: %w", p.path, err)
		}
		m.Entries = append(m.Entries, Entry{
			Path:       p.path,
			Size:       p.info.Size(),
			Executable: p.info.Mode()&0o111 != 0,
			SHA256:     sum,
		})
	}
	sort.Slice(m.Skipped, func(i, j int) bool { return m.Skipped[i].Path < m.Skipped[j].Path })
	return m, nil
}

func hashFile(fsys fs.FS, name string) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// ListWalk lists every regular-looking path under fsys, skipping any .git
// directory. It is the lister for a directory that is not a git checkout.
// Symlinks are listed (Build skips them with a reason) so the user is told.
func ListWalk(fsys fs.FS) ([]string, error) {
	var out []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		out = append(out, p)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// ListGit lists what git would consider part of the working tree in dir:
// tracked and untracked files, minus what the ignore files exclude
// (`git ls-files -co --exclude-standard`). A tracked file that was deleted is
// listed; Build reports it as missing.
func ListGit(ctx context.Context, dir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "ls-files", "-z", "-c", "-o", "--exclude-standard")
	// Run git against dir and nothing else: a GIT_DIR left in the environment
	// (by a hook, say) would make it list some other repository.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	var out []string
	for _, p := range strings.Split(string(b), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// List picks the lister. A directory with a .git entry is listed by git, and a
// git failure there is an error: falling back to a plain walk would send files
// the project's ignore rules keep out (build output, local credentials). A
// directory without one is walked.
func List(ctx context.Context, dir string) ([]string, error) {
	if _, err := os.Lstat(dir + "/.git"); err == nil {
		return ListGit(ctx, dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return ListWalk(os.DirFS(dir))
}
