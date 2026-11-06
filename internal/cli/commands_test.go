package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runners-io/cli/internal/transport"
	"github.com/runners-io/cli/internal/transport/fake"
)

// noSecrets fails if any of the secrets appears in any output of r.
func noSecrets(t *testing.T, r result, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(r.stdout, s) || strings.Contains(r.stderr, s) {
			t.Errorf("a secret reached the output: %q\nstdout: %s\nstderr: %s", s, r.stdout, r.stderr)
		}
	}
}

// ---- credentials ----

func TestNotSignedInIsRefusedLocallyBeforeAnyRequest(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	delete(h.env, "RUNNERS_TOKEN")
	r := h.run("warmup")
	if r.code != 123 || !strings.Contains(r.stderr, "login") {
		t.Fatalf("%+v", r)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("a request was made without a credential: %q", f.Calls())
	}
	// ANTI-VACUITY: with the token back the same command goes through.
	h.env["RUNNERS_TOKEN"] = testToken
	if r := h.run("warmup"); r.code != 0 {
		t.Fatalf("with a token: %+v", r)
	}
}

func TestTheServiceTokenBeatsTheStoredOneAndNeitherIsPrinted(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	h.store.tok = "rst_stored_personal"
	r := h.run("warmup", "--json")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	delete(h.env, "RUNNERS_TOKEN")
	if r2 := h.run("list", "--json"); r2.code != 0 {
		t.Fatalf("stored: %+v", r2)
	} else {
		noSecrets(t, r2, "rst_stored_personal", testToken)
	}
	got := f.Tokens()
	if len(got) != 2 || got[0] != testToken || got[1] != "rst_stored_personal" {
		t.Errorf("tokens that reached the transport = %q", got)
	}
	noSecrets(t, r, testToken, "rst_stored_personal")
}

func TestTheCredentialIsNeverInAnyOutputOfAnyCommand(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	dir := workspace(t, map[string]string{"a.txt": "1"})
	id := warm(t, h, f)
	for _, args := range [][]string{
		{"warmup", "--json"}, {"sync", "--id", id, "--dir", dir, "--json"},
		{"run", "--id", id, "--dir", dir, "--json", "--", "true"},
		{"list", "--json"}, {"list"}, {"extend", "--id", id, "--json"},
		{"status", "--id", id, "--json"}, {"stop", "--id", id, "--json"},
		{"stop", "--id", id, "--json"}, // session lost: an error event
		{"pull", "--id", "sm-9", "--dir", t.TempDir(), "x", "--json"},
	} {
		noSecrets(t, h.run(args...), testToken)
	}
}

// A transport that refuses the credential is exit 123, from the service.
func TestARefusedCredentialIs123(t *testing.T) {
	f := fake.New()
	f.SetRequiredToken("rst_the_real_one")
	h := newHarness(t, f)
	r := h.run("list")
	if r.code != 123 {
		t.Fatalf("%+v", r)
	}
	noSecrets(t, r, testToken)
}

// ---- login / logout ----

func TestLoginPollsStoresTheTokenAndPrintsOnlyTheUserCode(t *testing.T) {
	f := fake.New()
	f.SetPendingPolls(2)
	h := newHarness(t, f)
	delete(h.env, "RUNNERS_TOKEN")
	r := h.run("login", "--json")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	evs := r.events(t)
	if got := strings.Join(names(evs), ","); got != "login,signed_in" {
		t.Errorf("events = %s", got)
	}
	l := find(evs, "login")
	if l["user_code"] != fake.DeviceUserCode || l["url"] != "https://example.test/device" || l["expires_in"] != "15m0s" {
		t.Errorf("login event = %v", l)
	}
	if h.store.tok != fake.DeviceToken {
		t.Errorf("stored token = %q", h.store.tok)
	}
	noSecrets(t, r, fake.DeviceToken, fake.DeviceSecret)
	// Three polls, one wait before each, at the interval the service asked for.
	if len(h.slept) != 3 || h.slept[0] != 5*time.Second {
		t.Errorf("waits = %v", h.slept)
	}
	// Signed in now: a command works with no environment token.
	if r := h.run("list"); r.code != 0 {
		t.Errorf("after login: %+v", r)
	}
}

func TestLoginHumanOutputTellsAPersonWhatToDo(t *testing.T) {
	h := newHarness(t, fake.New())
	delete(h.env, "RUNNERS_TOKEN")
	r := h.run("login")
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.stderr, fake.DeviceUserCode) || !strings.Contains(r.stderr, "https://example.test/device") {
		t.Errorf("stderr should show the code and the address: %q", r.stderr)
	}
	noSecrets(t, r, fake.DeviceToken, fake.DeviceSecret)
}

func TestLoginGivesUpWhenTheCodeExpires(t *testing.T) {
	f := fake.New()
	f.SetPendingPolls(1 << 20)
	h := newHarness(t, f)
	delete(h.env, "RUNNERS_TOKEN")
	r := h.run("login", "--json")
	if r.code != 123 || !strings.Contains(r.stdout, "expired") {
		t.Fatalf("%+v", r)
	}
	if h.store.tok != "" {
		t.Error("a token was stored after an expired code")
	}
}

func TestLoginWhenTheStoreCannotSaveFailsWithoutTheToken(t *testing.T) {
	h := newHarness(t, fake.New())
	delete(h.env, "RUNNERS_TOKEN")
	h.store.saveErr = os.ErrPermission
	r := h.run("login")
	if r.code == 0 || !strings.Contains(r.stderr, "could not be stored") {
		t.Fatalf("%+v", r)
	}
	noSecrets(t, r, fake.DeviceToken)
}

func TestLoginWarnsWhenAnEnvironmentTokenWillShadowIt(t *testing.T) {
	h := newHarness(t, fake.New()) // RUNNERS_TOKEN is set
	r := h.run("login", "--json")
	if r.code != 0 || find(r.events(t), "warning") == nil {
		t.Fatalf("%+v", r)
	}
}

func TestLoginAgainstNoServiceIsNotReady(t *testing.T) {
	h := newHarness(t, transport.Unavailable{})
	if r := h.run("login"); r.code != 125 || h.store.tok != "" {
		t.Fatalf("%+v", r)
	}
}

func TestLogoutForgetsTheStoredTokenAndNeedsNoCredential(t *testing.T) {
	h := newHarness(t, fake.New())
	delete(h.env, "RUNNERS_TOKEN")
	h.store.tok = "rst_stored"
	if r := h.run("logout", "--json"); r.code != 0 || find(r.events(t), "signed_out") == nil {
		t.Fatalf("%+v", r)
	}
	if h.store.tok != "" {
		t.Error("the token is still stored")
	}
	if r := h.run("list"); r.code != 123 {
		t.Errorf("after logout: %+v", r)
	}
}

// ---- sync ----

func TestSyncSendsTheTreeAndNamesWhatItSkipped(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	dir := workspace(t, map[string]string{"main.go": "package x", ".env": "SECRET=1"})
	r := h.run("sync", "--id", id, "--dir", dir, "--json")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if _, ok := f.Files(id)["main.go"]; !ok {
		t.Errorf("main.go was not sent: %v", f.Files(id))
	}
	if _, ok := f.Files(id)[".env"]; ok {
		t.Error(".env was sent")
	}
	evs := r.events(t)
	if sk := find(evs, "skipped"); sk == nil || sk["path"] != ".env" || sk["reason"] != "secret" {
		t.Errorf("skipped event = %v", sk)
	}
	if find(evs, "exit") != nil {
		t.Error("sync ran a command")
	}
}

// ---- list ----

func TestListJSONAndHuman(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	if r := h.run("list"); r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "no running machines") {
		t.Errorf("empty human: %+v", r)
	}
	if r := h.run("list", "--json"); r.code != 0 || r.stdout != "" {
		t.Errorf("empty json must print nothing: %+v", r)
	}
	a, b := warm(t, h, f), warm(t, h, f)
	r := h.run("list", "--json")
	evs := r.events(t)
	if r.code != 0 || len(evs) != 2 || evs[0]["event"] != "status" || evs[0]["id"] != a || evs[1]["id"] != b {
		t.Fatalf("%+v", r)
	}
	if hr := h.run("list"); !strings.Contains(hr.stdout, a) || !strings.Contains(hr.stdout, b) || strings.Count(hr.stdout, "\n") != 2 {
		t.Errorf("human: %q", hr.stdout)
	}
}

// ---- extend ----

func TestExtendMovesTheIdleDeadlineAndStopsAtTheHardMaximum(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f) // ready 12:00, idle 10m, max 4h
	r := h.run("extend", "--id", id, "--by", "45m", "--json")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	e := find(r.events(t), "extended")
	if e == nil || e["idle_deadline"] != "2026-10-31T12:45:00Z" || e["hard_deadline"] != "2026-10-31T16:00:00Z" {
		t.Fatalf("extended = %v", e)
	}
	// Close to the hard maximum: capped, and the CLI says so.
	f.Advance(3*time.Hour + 30*time.Minute)
	r = h.run("extend", "--id", id, "--by", "60m", "--json")
	e = find(r.events(t), "extended")
	if e["idle_deadline"] != "2026-10-31T16:00:00Z" || find(r.events(t), "warning") == nil {
		t.Errorf("capped extend: %s", r.stdout)
	}
}

func TestExtendRefusesBadInputBeforeAnyRequestAndAGoneMachineIs124(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	before := len(f.Calls())
	for _, by := range []string{"30s", "2h", "0s", "soon", "-5m"} {
		if r := h.run("extend", "--id", id, "--by", by); r.code != 120 {
			t.Errorf("--by %s: %+v", by, r)
		}
	}
	if len(f.Calls()) != before {
		t.Error("a bad --by reached the service")
	}
	if r := h.run("extend", "--id", "sm-404"); r.code != 124 {
		t.Errorf("gone: %+v", r)
	}
}

// ---- pull ----

func TestPullWritesFilesAndKeepsTheExecutableBit(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	f.SetRemoteFiles([]transport.PullFile{
		{Path: "out/report.txt"},
		{Path: "bin/tool", Executable: true},
		{Path: "link", Mode: fs.ModeSymlink},
	}, map[string]string{"out/report.txt": "hello", "bin/tool": "#!/bin/sh\n", "link": "../../etc/passwd"})
	dest := t.TempDir()
	r := h.run("pull", "--id", id, "--dir", dest, "--json", "out", "bin")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "out", "report.txt")); string(b) != "hello" {
		t.Errorf("report = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(dest, "bin", "tool")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("tool: %v %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "link")); err == nil {
		t.Error("a symlink was written")
	}
	evs := r.events(t)
	if sk := find(evs, "skipped"); sk == nil || sk["reason"] != "not_regular" {
		t.Errorf("skipped = %v", sk)
	}
	if p := find(evs, "pulled"); p == nil || p["files"] != float64(4) { // two pulls deliver the same fake set: 2 regular x 2
		t.Errorf("pulled = %v", p)
	}
	// No temp file is left behind.
	_ = filepath.WalkDir(dest, func(p string, d fs.DirEntry, _ error) error {
		if strings.Contains(d.Name(), ".scratch-pull-") {
			t.Errorf("left behind: %s", p)
		}
		return nil
	})
}

func TestPullDoesNotTrustTheMachinesPaths(t *testing.T) {
	for _, bad := range []string{"../escape.txt", "/etc/escape.txt", "a/../../escape.txt", "a\\..\\x", ".", "", "a//b"} {
		f := fake.New()
		h := newHarness(t, f)
		id := warm(t, h, f)
		parent := t.TempDir()
		dest := filepath.Join(parent, "dest")
		f.SetRemoteFiles([]transport.PullFile{{Path: bad}}, map[string]string{bad: "pwned"})
		r := h.run("pull", "--id", id, "--dir", dest, "x")
		if r.code == 0 {
			t.Errorf("%q: pull succeeded", bad)
		}
		entries, _ := os.ReadDir(parent)
		for _, e := range entries {
			if e.Name() != "dest" {
				t.Errorf("%q: wrote %s next to the destination", bad, e.Name())
			}
		}
		if _, err := os.Stat("/etc/escape.txt"); err == nil {
			t.Fatal("wrote /etc/escape.txt")
		}
	}
}

// A symlink that already sits in the destination must not carry a write out.
func TestPullDoesNotFollowASymlinkOutOfTheDestination(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	dest := filepath.Join(parent, "dest")
	for _, d := range []string{outside, dest} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dest, "link")); err != nil {
		t.Skip("no symlinks")
	}
	f.SetRemoteFiles([]transport.PullFile{{Path: "link/evil.txt"}}, map[string]string{"link/evil.txt": "pwned"})
	r := h.run("pull", "--id", id, "--dir", dest, "x")
	if r.code == 0 {
		t.Errorf("pull through a symlink succeeded: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("the write escaped through the symlink")
	}
	// ANTI-VACUITY: the same file at a plain path is written.
	f.SetRemoteFiles([]transport.PullFile{{Path: "plain/ok.txt"}}, map[string]string{"plain/ok.txt": "fine"})
	if r := h.run("pull", "--id", id, "--dir", dest, "x"); r.code != 0 {
		t.Errorf("plain: %+v", r)
	}
}

func TestPullOverwritesAFileButNotThroughALinkToIt(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	target := filepath.Join(parent, "target.txt")
	_ = os.MkdirAll(dest, 0o755)
	_ = os.WriteFile(target, []byte("original"), 0o644)
	if err := os.Symlink(target, filepath.Join(dest, "f.txt")); err != nil {
		t.Skip("no symlinks")
	}
	f.SetRemoteFiles([]transport.PullFile{{Path: "f.txt"}}, map[string]string{"f.txt": "new"})
	if r := h.run("pull", "--id", id, "--dir", dest, "f.txt"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(target); string(b) != "original" {
		t.Errorf("the link's target was overwritten: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "f.txt")); string(b) != "new" {
		t.Errorf("f.txt = %q", b)
	}
}

func TestPullStopsAtTheSizeLimitAndLeavesNoPartialFile(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	h.max = 10
	id := warm(t, h, f)
	f.SetRemoteFiles([]transport.PullFile{{Path: "small"}, {Path: "big"}}, map[string]string{"small": "123456", "big": "0123456789"})
	dest := t.TempDir()
	r := h.run("pull", "--id", id, "--dir", dest, "x")
	if r.code != 121 {
		t.Fatalf("%+v", r)
	}
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		if e.Name() == "big" || strings.HasPrefix(e.Name(), ".scratch-pull-") {
			t.Errorf("left %s behind", e.Name())
		}
	}
}

func TestPullRefusesBadRemotePathsAndNoPath(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	before := len(f.Calls())
	for _, args := range [][]string{
		{"pull", "--id", id},
		{"pull", "--id", id, "/etc/passwd"},
		{"pull", "--id", id, "../x"},
		{"pull", "--id", id, "a/../../x"},
	} {
		if r := h.run(args...); r.code != 120 {
			t.Errorf("%v: %+v", args, r)
		}
	}
	if len(f.Calls()) != before {
		t.Error("a bad path reached the service")
	}
}

// ---- proxy ----

func TestProxyCarriesBytesBothWaysAndPrintsNothingElseOnStdout(t *testing.T) {
	f := fake.New()
	f.RelayReply = "SSH-2.0-box\r\n"
	h := newHarness(t, f)
	id := warm(t, h, f)
	r := h.runWithStdin("SSH-2.0-client\r\n", "scratch", "proxy", "--id", id)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if r.stdout != "SSH-2.0-box\r\n" {
		t.Errorf("stdout = %q: it must hold the machine's bytes and nothing else", r.stdout)
	}
	p := f.Relay()
	if p == nil || p.Sent() != "SSH-2.0-client\r\n" || !p.Closed() {
		t.Errorf("relay: %+v", p)
	}
	noSecrets(t, r, testToken)
}

func TestProxyRefusesAnIDThatCouldBeAnSSHOption(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	before := len(f.Calls())
	for _, id := range []string{"-oProxyCommand=x", "a b", "A"} {
		if r := h.run("proxy", "--id", id); r.code != 120 || r.stdout != "" {
			t.Errorf("%q: %+v", id, r)
		}
	}
	if len(f.Calls()) != before {
		t.Error("a bad id reached the service")
	}
}

func TestProxyOnAGoneMachineIs124(t *testing.T) {
	h := newHarness(t, fake.New())
	if r := h.run("proxy", "--id", "sm-404"); r.code != 124 || r.stdout != "" {
		t.Fatalf("%+v", r)
	}
}

// ---- the remote exit code ----

func TestSyncAndRunAreSeparateSoRunCanSkipTheSync(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	f.ExecFunc = func(transport.ExecRequest, transport.Streams) int { return 7 }
	r := h.run("run", "--id", id, "--no-sync", "--json", "--", "false")
	if r.code != 7 {
		t.Fatalf("remote exit 7 must be the process exit code: %+v", r)
	}
	if e := find(r.events(t), "exit"); e["code"] != float64(7) || e["remote"] != true {
		t.Errorf("exit event = %v", e)
	}
}
