package sshsync

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidID(t *testing.T) {
	for _, ok := range []string{"sm-1", "a", "0abc", strings.Repeat("a", 63)} {
		if err := ValidID(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-oProxyCommand=x", "-1", "a b", "a;b", "A", "a/b", "a$(x)", "a\nb", strings.Repeat("a", 64), "sm_1", "a'b"} {
		if err := ValidID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestArgsNameTheMachineAndNeverCarryAFileListOrASecret(t *testing.T) {
	a, err := Args(Options{ID: "sm-1", Dir: "/home/me/proj/", ConfigPath: "/tmp/x/ssh_config"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-rtp", "--files-from=-", "--from0", "-e", "ssh -F /tmp/x/ssh_config", "/home/me/proj/", "runner@sm-1:/workspace/"}
	if strings.Join(a, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args = %q\nwant   %q", a, want)
	}
	// The signature takes no credential, so none can be here; this holds the
	// line against someone adding one.
	for _, x := range a {
		if strings.Contains(strings.ToLower(x), "token") {
			t.Errorf("argument %q looks like a credential", x)
		}
	}
}

func TestArgsRefuseWhatWouldBeParsedAsAnOption(t *testing.T) {
	cases := []Options{
		{ID: "-oProxyCommand=evil", Dir: "d", ConfigPath: "/c"},
		{ID: "sm-1", Dir: "", ConfigPath: "/c"},
		{ID: "sm-1", Dir: "d", ConfigPath: ""},
		{ID: "sm-1", Dir: "d", ConfigPath: "/a b/c"},
		{ID: "sm-1", Dir: "d", ConfigPath: "/a'b/c"},
	}
	for _, o := range cases {
		if _, err := Args(o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

// The ProxyCommand line is run by a shell. Run it with a stand-in for this
// program whose path has a space and a quote in it, and see that the program
// receives exactly `scratch proxy --id sm-1` and nothing else.
func TestTheProxyCommandSurvivesAHostilePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	dir := filepath.Join(t.TempDir(), "my dir", "it's")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(dir, "runners")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '[%s]' \"$a\"; done\n"
	if err := os.WriteFile(self, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := Config(self)
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(cfg, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "ProxyCommand "); ok {
			line = strings.ReplaceAll(rest, "%h", "sm-1") // what ssh substitutes
		}
	}
	if line == "" {
		t.Fatalf("no ProxyCommand in:\n%s", cfg)
	}
	out, err := exec.Command("sh", "-c", line).Output()
	if err != nil {
		t.Fatalf("running %q: %v", line, err)
	}
	if string(out) != "[scratch][proxy][--id][sm-1]" {
		t.Errorf("the program received %q", out)
	}
	if _, err := Config("/a\nb"); err == nil {
		t.Error("a path with a newline was accepted")
	}
}

// Check the flag prefix against the rsync on this machine (openrsync on a Mac,
// rsync 3.x on Linux): a path with a space, a leading dash and a newline must
// arrive, and a file NOT in the list must not.
func TestTheFlagsDoWhatTheyClaimOnTheRsyncAtHand(t *testing.T) {
	rs, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync is not installed here")
	}
	src, dst := t.TempDir(), t.TempDir()
	files := map[string]string{"a.txt": "a", "sub dir/b.txt": "b", "-dash": "d", "new\nline": "n", "left-out": "x"}
	for name, data := range files {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list := "a.txt\x00sub dir/b.txt\x00-dash\x00new\nline\x00"
	cmd := exec.Command(rs, append(FlagPrefix(), src+"/", dst+"/")...)
	cmd.Stdin = strings.NewReader(list)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rsync: %v\n%s", err, out)
	}
	for _, name := range []string{"a.txt", "sub dir/b.txt", "-dash", "new\nline"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("%q did not arrive: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "left-out")); err == nil {
		t.Error("a file that was not in the list was sent")
	}
}
