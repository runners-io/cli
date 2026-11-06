package manifest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func write(t *testing.T, root, rel, data string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A git checkout is listed by git, so the project's ignore rules decide what
// is sent, and a GIT_DIR in the environment (as a hook leaves) cannot point it
// at another repository.
func TestListUsesGitIgnoreRulesInACheckout(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	write(t, root, ".gitignore", "node_modules/\n*.log\n")
	write(t, root, "src/main.go", "package main")
	write(t, root, "tracked.txt", "t")
	write(t, root, "untracked.txt", "u")
	write(t, root, "node_modules/dep/index.js", "x")
	write(t, root, "build.log", "x")
	write(t, root, ".env", "A=1")
	git(t, root, "add", "tracked.txt", ".gitignore")
	// Deleted after being added: still in the index, gone from disk.
	write(t, root, "deleted.txt", "d")
	git(t, root, "add", "deleted.txt")
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "not-this-repo"))
	got, err := List(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".env", ".gitignore", "deleted.txt", "src/main.go", "tracked.txt", "untracked.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %v\nwant   %v", got, want)
	}

	// And Build over that listing: the secret and the vanished file are named,
	// the ignored directory never appears.
	m, err := Build(os.DirFS(root), got, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sk := skips(m.Skipped)
	if sk[".env"] != ReasonSecret || sk["deleted.txt"] != ReasonMissing || len(sk) != 2 {
		t.Fatalf("skips = %v", sk)
	}
	if !reflect.DeepEqual(paths(m.Entries), []string{".gitignore", "src/main.go", "tracked.txt", "untracked.txt"}) {
		t.Fatalf("entries = %v", paths(m.Entries))
	}
}

func TestListWalksADirectoryThatIsNotACheckout(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.txt", "1")
	write(t, root, "d/b.txt", "2")
	got, err := List(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a.txt", "d/b.txt"}) {
		t.Fatalf("List = %v", got)
	}
}

// A checkout whose git fails must fail the listing, not fall back to a walk
// that would send what the ignore rules exclude.
func TestListFailsClosedWhenGitFailsInACheckout(t *testing.T) {
	root := t.TempDir()
	// A .git that is not a repository: git ls-files cannot read it.
	write(t, root, ".git/HEAD", "garbage")
	write(t, root, "a.txt", "1")
	if _, err := List(context.Background(), root); err == nil {
		t.Fatal("List fell back to a walk in a directory with a broken .git")
	}
}
