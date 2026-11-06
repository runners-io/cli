package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const secret = "rst_secret_value_do_not_leak"

func TestSaveThenLoadRoundTripsAndTheFileIsPrivate(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "runners", "credentials")}
	if _, err := f.Load(); !errors.Is(err, ErrNone) {
		t.Fatalf("empty store: %v", err)
	}
	if err := f.Save(secret); err != nil {
		t.Fatal(err)
	}
	got, err := f.Load()
	if err != nil || got != secret {
		t.Fatalf("Load = %q, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(f.Path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode %o, want 600", fi.Mode().Perm())
		}
		di, _ := os.Stat(filepath.Dir(f.Path))
		if di.Mode().Perm() != 0o700 {
			t.Errorf("directory mode %o, want 700", di.Mode().Perm())
		}
	}
	// Saving again replaces, and leaves no temp file behind.
	if err := f.Save("second"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.Load(); got != "second" {
		t.Errorf("after replace Load = %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(f.Path))
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries after two saves, want 1", len(entries))
	}
}

func TestLoadRefusesAFileOtherUsersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	f := File{Path: filepath.Join(t.TempDir(), "credentials")}
	if err := os.WriteFile(f.Path, []byte(`{"token":"`+secret+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := f.Load()
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a world-readable file loaded: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("the error quotes the token")
	}
	// ANTI-VACUITY: the same content at 0600 does load.
	if err := os.Chmod(f.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := f.Load(); err != nil || got != secret {
		t.Fatalf("0600 file: %q %v", got, err)
	}
}

func TestLoadRefusesASymlinkAndGarbageWithoutQuotingIt(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks")
	}
	if _, err := (File{Path: link}).Load(); err == nil {
		t.Error("a symlinked credentials file loaded")
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("not json "+secret), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := File{Path: bad}.Load()
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestDeleteAndEmptyToken(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "c")}
	if err := f.Delete(); err != nil {
		t.Errorf("deleting nothing: %v", err)
	}
	if err := f.Save("  "); err == nil {
		t.Error("an empty token was stored")
	}
	_ = f.Save(secret)
	if err := f.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(); !errors.Is(err, ErrNone) {
		t.Errorf("after delete: %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	if got := DefaultPath(env(map[string]string{"XDG_CONFIG_HOME": "/x", "HOME": "/h"})); got != filepath.Join("/x", "runners", "credentials") {
		t.Errorf("xdg: %s", got)
	}
	if got := DefaultPath(env(map[string]string{"HOME": "/h"})); got != filepath.Join("/h", ".config", "runners", "credentials") {
		t.Errorf("home: %s", got)
	}
	if got := DefaultPath(env(nil)); got != "" {
		t.Errorf("none: %q", got)
	}
}
