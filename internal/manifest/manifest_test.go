package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/runners-io/cli/internal/platform"
)

func f(data string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(data), Mode: 0o644} }

func paths(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Path)
	}
	return out
}

func skips(ss []Skip) map[string]string {
	out := map[string]string{}
	for _, s := range ss {
		out[s.Path] = s.Reason
	}
	return out
}

func TestBuildHashesSortsAndRecordsMode(t *testing.T) {
	fsys := fstest.MapFS{
		"b.txt":      f("bee"),
		"a.go":       f("package a"),
		"bin/run.sh": {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
	}
	m, err := Build(fsys, []string{"b.txt", "bin/run.sh", "a.go"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(m.Entries); !reflect.DeepEqual(got, []string{"a.go", "b.txt", "bin/run.sh"}) {
		t.Fatalf("entries not sorted by path: %v", got)
	}
	sum := sha256.Sum256([]byte("bee"))
	if m.Entries[1].SHA256 != hex.EncodeToString(sum[:]) || m.Entries[1].Size != 3 {
		t.Errorf("b.txt entry wrong: %+v", m.Entries[1])
	}
	if !m.Entries[2].Executable || m.Entries[0].Executable {
		t.Errorf("executable bit wrong: %+v", m.Entries)
	}
	if m.TotalBytes != int64(len("bee")+len("package a")+len("#!/bin/sh\n")) {
		t.Errorf("TotalBytes = %d", m.TotalBytes)
	}
}

func TestDigestIsStableAndSensitive(t *testing.T) {
	base := fstest.MapFS{"a": f("1"), "b": f("2")}
	m1, _ := Build(base, []string{"a", "b"}, Options{})
	m2, _ := Build(base, []string{"b", "a", "a"}, Options{}) // order and duplicates do not matter
	if m1.Digest() != m2.Digest() {
		t.Fatal("digest depends on candidate order or duplicates")
	}
	changed := fstest.MapFS{"a": f("1"), "b": f("3")}
	m3, _ := Build(changed, []string{"a", "b"}, Options{})
	if m1.Digest() == m3.Digest() {
		t.Fatal("digest did not change when a file's content changed")
	}
	chmod := fstest.MapFS{"a": f("1"), "b": {Data: []byte("2"), Mode: 0o755}}
	m4, _ := Build(chmod, []string{"a", "b"}, Options{})
	if m1.Digest() == m4.Digest() {
		t.Fatal("digest did not change when the executable bit changed")
	}
}

// A path containing the framing characters must not be able to collide with
// two other entries.
func TestDigestFramingIsUnambiguous(t *testing.T) {
	a := Manifest{Entries: []Entry{{Path: "x\ny", Size: 1, SHA256: "aa"}}}
	b := Manifest{Entries: []Entry{{Path: "x", Size: 1, SHA256: "aa"}, {Path: "y", Size: 1, SHA256: "aa"}}}
	if a.Digest() == b.Digest() {
		t.Fatal("digest collides across different entry sets")
	}
}

func TestSecretShapedFilesAreSkippedAndNamed(t *testing.T) {
	secret := []string{
		".env", ".env.local", ".env.production", "app/.env", "app/.ENV.Test",
		"certs/server.pem", "certs/server.KEY", "id.p12", "id.pfx",
		"home/.ssh/config", "home/.aws/credentials", ".gnupg/pubring.kbx",
		"id_rsa", "keys/id_ed25519", "id_ecdsa", "id_dsa",
		".netrc", ".git-credentials",
		"infra/terraform.tfstate", "infra/terraform.tfstate.backup",
	}
	notSecret := []string{
		"environment.go", "env/readme.md", "id_rsa.pub", "keys/id_ed25519.pub", "keyboard.go",
		"pem.go", "monkey.txt", "src/.github/ci.yml", ".gitignore", "tfstate.md",
	}
	fsys := fstest.MapFS{}
	var cands []string
	for _, p := range append(append([]string{}, secret...), notSecret...) {
		fsys[p] = f("x")
		cands = append(cands, p)
	}
	m, err := Build(fsys, cands, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sk := skips(m.Skipped)
	for _, p := range secret {
		if sk[p] != ReasonSecret {
			t.Errorf("%s was not skipped as a secret (got %q)", p, sk[p])
		}
	}
	got := map[string]bool{}
	for _, e := range m.Entries {
		got[e.Path] = true
	}
	for _, p := range notSecret {
		if !got[p] {
			t.Errorf("%s was wrongly dropped (skip reason %q)", p, sk[p])
		}
	}
	if len(m.Entries)+len(m.Skipped) != len(cands) {
		t.Errorf("every candidate must be either sent or skipped: %d + %d != %d", len(m.Entries), len(m.Skipped), len(cands))
	}
}

// ANTI-VACUITY: the secret rules above would pass if Build skipped
// everything. Prove the same Build sends an ordinary file, and that the
// classifier is what dropped the secret one.
func TestSecretRuleIsTheOnlyReasonTheEnvFileIsDropped(t *testing.T) {
	fsys := fstest.MapFS{"main.go": f("x"), ".env": f("TOKEN=hunter2")}
	m, _ := Build(fsys, []string{"main.go", ".env"}, Options{})
	if !reflect.DeepEqual(paths(m.Entries), []string{"main.go"}) {
		t.Fatalf("sent %v", paths(m.Entries))
	}
	if secret, _ := IsSecretPath(".env"); !secret {
		t.Fatal("IsSecretPath(.env) is false")
	}
	if secret, _ := IsSecretPath("main.go"); secret {
		t.Fatal("IsSecretPath(main.go) is true")
	}
}

func TestSecretContentIsNeverHashed(t *testing.T) {
	// The digest of a skipped file's content must not be derivable from the
	// manifest, which can end up in a log.
	fsys := fstest.MapFS{".env": f("TOKEN=hunter2")}
	m, _ := Build(fsys, []string{".env"}, Options{})
	sum := sha256.Sum256([]byte("TOKEN=hunter2"))
	if strings.Contains(fmt.Sprintf("%+v", m), hex.EncodeToString(sum[:])) {
		t.Fatal("the manifest carries a hash of a secret file")
	}
}

func TestAllowOverridesOnePathOnly(t *testing.T) {
	fsys := fstest.MapFS{".env.example": f("A=1"), ".env": f("B=2"), "k.pem": f("c")}
	cands := []string{".env.example", ".env", "k.pem"}
	m, err := Build(fsys, cands, Options{Allow: []string{".env.example", "nonexistent.pem"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths(m.Entries), []string{".env.example"}) {
		t.Fatalf("sent %v, want only the allowed file", paths(m.Entries))
	}
	sk := skips(m.Skipped)
	if sk[".env"] != ReasonSecret || sk["k.pem"] != ReasonSecret {
		t.Errorf("an --allow leaked to other files: %v", sk)
	}
	// A typo'd --allow must be visible, not silently ignored.
	if !reflect.DeepEqual(m.UnusedAllow, []string{"nonexistent.pem"}) {
		t.Errorf("UnusedAllow = %v", m.UnusedAllow)
	}
}

func TestAllowDoesNotOverrideSymlinkOrGit(t *testing.T) {
	fsys := fstest.MapFS{
		"link":        {Data: []byte("../outside"), Mode: fs.ModeSymlink},
		".git/config": f("[remote]"),
	}
	m, _ := Build(fsys, []string{"link", ".git/config"}, Options{Allow: []string{"link", ".git/config"}})
	if len(m.Entries) != 0 {
		t.Fatalf("allow sent %v", paths(m.Entries))
	}
}

func TestGitDirSymlinkMissingAndNonRegularAreSkippedWithReasons(t *testing.T) {
	fsys := fstest.MapFS{
		".git/config":  f("[remote]"),
		".git/HEAD":    f("ref"),
		"link":         {Data: []byte("../outside"), Mode: fs.ModeSymlink},
		"sub/.git/x":   f("nested"),
		"sub/dir/file": f("ok"),
		"fifo":         {Mode: fs.ModeNamedPipe},
	}
	cands := []string{".git/config", ".git/HEAD", "link", "gone.txt", "sub/.git/x", "sub/dir", "sub/dir/file", "fifo"}
	m, err := Build(fsys, cands, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sk := skips(m.Skipped)
	want := map[string]string{
		".git/config": ReasonGit, ".git/HEAD": ReasonGit, "sub/.git/x": ReasonGit,
		"link": ReasonSymlink, "gone.txt": ReasonMissing, "sub/dir": ReasonNotRegular, "fifo": ReasonNotRegular,
	}
	if !reflect.DeepEqual(sk, want) {
		t.Fatalf("skips = %v\nwant    %v", sk, want)
	}
	if !reflect.DeepEqual(paths(m.Entries), []string{"sub/dir/file"}) {
		t.Fatalf("sent %v", paths(m.Entries))
	}
}

func TestInvalidCandidatePathsAreRefused(t *testing.T) {
	fsys := fstest.MapFS{"a": f("1")}
	for _, bad := range []string{"../a", "/etc/passwd", "a/../../b", "", ".", "a//b", `a\b`} {
		_, err := Build(fsys, []string{bad}, Options{})
		if k, ok := platform.KindOf(err); !ok || k != platform.KindUsage {
			t.Errorf("path %q: err = %v, want a usage error", bad, err)
		}
	}
}

func TestWorkspaceLimitRefusesBeforeHashing(t *testing.T) {
	fsys := fstest.MapFS{"a": f(strings.Repeat("x", 600)), "b": f(strings.Repeat("y", 600))}
	_, err := Build(fsys, []string{"a", "b"}, Options{MaxWorkspaceBytes: 1000})
	k, ok := platform.KindOf(err)
	if !ok || k != platform.KindSyncTooLarge {
		t.Fatalf("err = %v, want a sync-too-large error", err)
	}
	if !strings.Contains(err.Error(), "2 files") {
		t.Errorf("the message should say how big and how many: %v", err)
	}
	// Exactly at the limit is allowed.
	if _, err := Build(fsys, []string{"a", "b"}, Options{MaxWorkspaceBytes: 1200}); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	// Skipped files do not count towards the limit.
	big := fstest.MapFS{".env": f(strings.Repeat("z", 5000)), "a": f("1")}
	if _, err := Build(big, []string{".env", "a"}, Options{MaxWorkspaceBytes: 1000}); err != nil {
		t.Fatalf("a skipped secret counted against the limit: %v", err)
	}
}

func TestDefaultLimitIsTwoGiB(t *testing.T) {
	if DefaultMaxWorkspaceBytes != 2<<30 {
		t.Fatalf("DefaultMaxWorkspaceBytes = %d", DefaultMaxWorkspaceBytes)
	}
}

func TestListWalkSkipsGitAndReturnsRegularFilePaths(t *testing.T) {
	fsys := fstest.MapFS{
		"a.txt":       f("1"),
		"d/b.txt":     f("2"),
		".git/config": f("x"),
		"d/.git/x":    f("y"),
	}
	got, err := ListWalk(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a.txt", "d/b.txt"}) {
		t.Fatalf("ListWalk = %v", got)
	}
}
