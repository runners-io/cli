package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/runners-io/cli/internal/credentials"
	"github.com/runners-io/cli/internal/platform"
	"github.com/runners-io/cli/internal/transport"
	"github.com/runners-io/cli/internal/transport/fake"
)

// result is one CLI invocation.
type result struct {
	code   int
	stdout string
	stderr string
}

func (r result) events(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(r.stdout))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q (%v)\nstdout:\n%s", sc.Text(), err, r.stdout)
		}
		out = append(out, m)
	}
	return out
}

func names(evs []map[string]any) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e["event"].(string))
	}
	return out
}

func find(evs []map[string]any, name string) map[string]any {
	for _, e := range evs {
		if e["event"] == name {
			return e
		}
	}
	return nil
}

type harness struct {
	t     *testing.T
	tr    transport.Transport
	env   map[string]string
	max   int64
	now   time.Time
	store *memStore
	slept []time.Duration
}

// memStore is an in-memory credentials.Store.
type memStore struct {
	tok     string
	saveErr error
}

func (m *memStore) Load() (string, error) {
	if m.tok == "" {
		return "", credentials.ErrNone
	}
	return m.tok, nil
}
func (m *memStore) Save(t string) error { m.tok = t; return m.saveErr }
func (m *memStore) Delete() error       { m.tok = ""; return nil }

const testToken = "rst_test_token_must_never_print"

// newHarness signs the caller in with a service token, as an agent would; the
// sign-in tests clear it.
func newHarness(t *testing.T, tr transport.Transport) *harness {
	return &harness{t: t, tr: tr, env: map[string]string{"RUNNERS_TOKEN": testToken},
		now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), store: &memStore{}}
}

// run is `runners scratch <args>`: every command of this tool lives under the
// scratch verb.
func (h *harness) run(args ...string) result {
	return h.runWithStdin("", append([]string{"scratch"}, args...)...)
}

// runTop is `runners <args>`, with no verb added.
func (h *harness) runTop(args ...string) result { return h.runWithStdin("", args...) }

func (h *harness) runWithStdin(stdin string, args ...string) result {
	h.t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), Env{
		Args:   args,
		Stdin:  strings.NewReader(stdin),
		Stdout: &out,
		Stderr: &errb,
		LookupEnv: func(k string) (string, bool) {
			v, ok := h.env[k]
			return v, ok
		},
		Now: func() time.Time { return h.now },
		Sleep: func(_ context.Context, d time.Duration) error {
			h.slept = append(h.slept, d)
			h.now = h.now.Add(d)
			return nil
		},
		Credentials:       h.store,
		Transport:         h.tr,
		MaxWorkspaceBytes: h.max,
	})
	return result{code, out.String(), errb.String()}
}

// workspace makes a directory with the given files and returns its path.
func workspace(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func warm(t *testing.T, h *harness, f *fake.Fake) string {
	t.Helper()
	b, err := f.Warmup(context.Background(), transport.WarmupRequest{Size: 2, Image: "ubuntu-2404", IdleTimeout: 10 * time.Minute, MaxDuration: 4 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return b.ID
}

func TestHelpVersionAndUnknownCommand(t *testing.T) {
	h := newHarness(t, fake.New())
	for _, a := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		r := h.run(a...)
		if r.code != 0 || !strings.Contains(r.stdout, "warmup") || !strings.Contains(r.stdout, "Exit codes") {
			t.Errorf("%v: code %d stdout %q", a, r.code, r.stdout)
		}
	}
	// Every verb in the command table is in the help text.
	help := h.run("help").stdout
	for _, c := range Commands() {
		if !strings.Contains(help, c.Name) {
			t.Errorf("help does not mention %q", c.Name)
		}
	}
	if r := h.run(); r.code != 120 || r.stdout != "" || !strings.Contains(r.stderr, "Usage") {
		t.Errorf("no args: %+v", r)
	}
	if r := h.run("frobnicate"); r.code != 120 || !strings.Contains(r.stderr, `unknown command "frobnicate"`) {
		t.Errorf("unknown: %+v", r)
	}
	if r := h.runTop("version"); r.code != 0 || !strings.HasPrefix(r.stdout, "runners ") {
		t.Errorf("version: %+v", r)
	}
	if r := h.runTop("--version"); r.code != 0 || !strings.HasPrefix(r.stdout, "runners ") {
		t.Errorf("--version: %+v", r)
	}
}

// The binary is `runners` and the only verb it has is `scratch`; everything the
// old runners-scratch binary did is `runners scratch <command>` (RUN-2421).
func TestTheBinaryIsRunnersAndEveryCommandIsUnderScratch(t *testing.T) {
	h := newHarness(t, fake.New())
	if r := h.runTop(); r.code != 120 || r.stdout != "" || !strings.Contains(r.stderr, "Usage: runners <command>") {
		t.Errorf("no args: %+v", r)
	}
	top := h.runTop("help")
	if top.code != 0 || !strings.Contains(top.stdout, "Usage: runners <command>") || !strings.Contains(top.stdout, "scratch") {
		t.Errorf("top help: %+v", top)
	}
	sub := h.runTop("scratch", "help")
	if sub.code != 0 || !strings.Contains(sub.stdout, "Usage: runners scratch <command>") {
		t.Errorf("scratch help: %+v", sub)
	}
	if strings.Contains(sub.stdout, "runners-scratch") || strings.Contains(top.stdout, "runners-scratch") {
		t.Error("help still names the old binary")
	}
	// A bare `runners scratch` is a usage error, and a command without the verb is unknown.
	if r := h.runTop("scratch"); r.code != 120 || !strings.Contains(r.stderr, "Usage: runners scratch <command>") {
		t.Errorf("bare scratch: %+v", r)
	}
	r := h.runTop("warmup")
	if r.code != 120 || !strings.Contains(r.stderr, `unknown command "warmup"`) || !strings.Contains(r.stderr, "runners help") {
		t.Errorf("a command outside scratch: %+v", r)
	}
	// ANTI-VACUITY: the same command under the verb does work.
	if r := h.run("warmup"); r.code != 0 {
		t.Errorf("runners scratch warmup: %+v", r)
	}
}

func TestCommandTableIsTheVerbsOfTheDesign(t *testing.T) {
	var got []string
	for _, c := range Commands() {
		got = append(got, c.Name)
	}
	want := []string{"login", "logout", "warmup", "sync", "run", "ssh", "pull", "list", "extend", "stop", "status", "proxy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestWarmupDefaultsAndHumanOutput(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	r := h.run("warmup")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	// stdout is the id and nothing else, so `ID=$(runners scratch warmup)` works.
	if r.stdout != "sm-1\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
	if !strings.Contains(r.stderr, "ready") {
		t.Errorf("stderr should narrate: %q", r.stderr)
	}
	want := "Warmup size=2 image=ubuntu-2404 idle=10m0s max=4h0m0s"
	if got := f.Calls(); len(got) != 1 || got[0] != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestWarmupFlagsAreForwarded(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	r := h.run("warmup", "--size", "8", "--idle-timeout", "30m", "--max-duration", "2h")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	want := "Warmup size=8 image=ubuntu-2404 idle=30m0s max=2h0m0s"
	if got := f.Calls()[0]; got != want {
		t.Errorf("call = %q, want %q", got, want)
	}
}

func TestWarmupRefusesBadInputBeforeAnyRequest(t *testing.T) {
	cases := [][]string{
		{"warmup", "--size", "3"},
		{"warmup", "--size", "16"},
		{"warmup", "--idle-timeout", "1m"},
		{"warmup", "--idle-timeout", "61m"},
		{"warmup", "--max-duration", "5h"},
		{"warmup", "--max-duration", "0s"},
		{"warmup", "--image", "windows-2025"},
		{"warmup", "--idle-timeout", "soon"},
		{"warmup", "stray"},
		{"warmup", "--no-such-flag"},
	}
	for _, args := range cases {
		f := fake.New()
		r := newHarness(t, f).run(args...)
		if r.code != 120 {
			t.Errorf("%v: code %d, want 120 (stderr %q)", args, r.code, r.stderr)
		}
		if len(f.Calls()) != 0 {
			t.Errorf("%v: a request was made: %q", args, f.Calls())
		}
		if !strings.Contains(r.stderr, "error:") {
			t.Errorf("%v: no explanation on stderr: %q", args, r.stderr)
		}
	}
}

func TestWarmupJSONEmitsOnlyNDJSONOnStdout(t *testing.T) {
	h := newHarness(t, fake.New())
	r := h.run("warmup", "--json", "--size", "4")
	if r.code != 0 || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	evs := r.events(t)
	if !reflect.DeepEqual(names(evs), []string{"warmup", "ready"}) {
		t.Fatalf("events = %v", names(evs))
	}
	ready := find(evs, "ready")
	if ready["id"] != "sm-1" || ready["size"] != float64(4) || ready["v"] != float64(1) || ready["ts"] != "2026-10-31T12:00:00Z" {
		t.Errorf("ready = %v", ready)
	}
}

func TestPlatformRefusalIsAnErrorEventAndTheKindsExitCode(t *testing.T) {
	f := fake.New()
	f.MaxBoxes = 0
	h := newHarness(t, f)

	r := h.run("warmup", "--json")
	if r.code != 122 {
		t.Fatalf("code %d", r.code)
	}
	e := find(r.events(t), "error")
	if e == nil || e["kind"] != "quota_or_billing" || e["code"] != float64(122) || !strings.Contains(e["message"].(string), "already has") {
		t.Fatalf("error event = %v", e)
	}

	r = h.run("warmup")
	if r.code != 122 || r.stdout != "" || !strings.Contains(r.stderr, "\nerror: the organisation already has") {
		t.Fatalf("human: %+v", r)
	}
}

func TestRunSyncsThenExecsAndPassesTheExitCodeThrough(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	dir := workspace(t, map[string]string{"main.go": "package main", "sub/x.txt": "x"})
	f.ExecFunc = func(req transport.ExecRequest, s transport.Streams) int {
		s.Stdout.Write([]byte("built\n"))
		s.Stderr.Write([]byte("careful\n"))
		return 7
	}

	r := h.run("run", "--id", id, "--dir", dir, "--", "make", "test")
	if r.code != 7 {
		t.Fatalf("exit code %d, want the remote command's 7: %+v", r.code, r)
	}
	// Program output is passed through untouched: stdout is the program's stdout.
	if r.stdout != "built\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
	if !strings.Contains(r.stderr, "careful\n") || !strings.Contains(r.stderr, "syncing 2 files") {
		t.Errorf("stderr = %q", r.stderr)
	}
	calls := f.Calls()
	if len(calls) != 3 || !strings.HasPrefix(calls[1], "Sync "+id) || !strings.HasPrefix(calls[2], "Exec "+id) {
		t.Fatalf("calls = %q", calls)
	}
	if got := f.LastExec().Argv; !reflect.DeepEqual(got, []string{"make", "test"}) {
		t.Errorf("argv = %v", got)
	}
	if files := f.Files(id); len(files) != 2 || string(files["sub/x.txt"].Data) != "x" {
		t.Errorf("box files = %v", files)
	}
}

func TestRunJSONEventOrderAndFraming(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	dir := workspace(t, map[string]string{"a.txt": "1", ".env": "TOKEN=x"})
	f.ExecFunc = func(_ transport.ExecRequest, s transport.Streams) int {
		s.Stdout.Write([]byte("two\nlines\n"))
		s.Stderr.Write([]byte("oops\n"))
		return 3
	}
	r := h.run("run", "--json", "--id", id, "--dir", dir, "echo")
	if r.code != 3 || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	evs := r.events(t)
	if !reflect.DeepEqual(names(evs), []string{"skipped", "syncing", "stdout", "stderr", "exit"}) {
		t.Fatalf("events = %v", names(evs))
	}
	if sk := evs[0]; sk["path"] != ".env" || sk["reason"] != "secret" {
		t.Errorf("skipped = %v", sk)
	}
	if sy := evs[1]; sy["files"] != float64(1) || sy["skipped"] != float64(1) || sy["bytes"] != float64(1) || len(sy["digest"].(string)) != 64 {
		t.Errorf("syncing = %v", sy)
	}
	if evs[2]["data"] != "two\nlines\n" || evs[3]["data"] != "oops\n" {
		t.Errorf("stream events = %v %v", evs[2], evs[3])
	}
	if ex := evs[4]; ex["code"] != float64(3) || ex["remote"] != true {
		t.Errorf("exit = %v", ex)
	}
}

func TestRunNeverSendsASecretAndAllowSendsExactlyThatFile(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	dir := workspace(t, map[string]string{"a.txt": "1", ".env": "TOKEN=hunter2", "k.pem": "KEY", "app/.env.local": "X=1"})

	r := h.run("run", "--id", id, "--dir", dir, "true")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := f.Files(id); len(got) != 1 {
		t.Fatalf("box holds %v, want only a.txt", got)
	}
	for _, want := range []string{"skipped .env (secret)", "skipped k.pem (secret)", "skipped app/.env.local (secret)", "3 skipped"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}

	r = h.run("run", "--id", id, "--dir", dir, "--allow", "./.env", "--allow", "typo.pem", "true")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	files := f.Files(id)
	if len(files) != 2 || string(files[".env"].Data) != "TOKEN=hunter2" {
		t.Fatalf("box holds %v, want a.txt and .env", files)
	}
	if _, ok := files["k.pem"]; ok {
		t.Fatal("--allow .env also sent k.pem")
	}
	if !strings.Contains(r.stderr, "typo.pem") {
		t.Errorf("an --allow that matched nothing was not reported:\n%s", r.stderr)
	}
}

func TestRunNoSyncSkipsTheSync(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	r := h.run("run", "--id", id, "--no-sync", "uname")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "Sync") {
			t.Fatalf("--no-sync made a sync call: %q", f.Calls())
		}
	}
}

func TestRunIDComesFromTheFlagOrTheEnvironment(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)

	if r := h.run("run", "--no-sync", "true"); r.code != 120 || !strings.Contains(r.stderr, "--id") {
		t.Errorf("no id: %+v", r)
	}
	h.env["RUNNERS_SCRATCH_ID"] = id
	if r := h.run("run", "--no-sync", "true"); r.code != 0 {
		t.Errorf("id from env: %+v", r)
	}
	// The flag wins over the environment.
	if r := h.run("run", "--id", "sm-nope", "--no-sync", "true"); r.code != 124 {
		t.Errorf("flag should win: %+v", r)
	}
}

func TestRunNeedsACommandAndReportsAGoneSession(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	if r := h.run("run", "--id", id); r.code != 120 || !strings.Contains(r.stderr, "command") {
		t.Errorf("no command: %+v", r)
	}
	f.Expire(id)
	r := h.run("run", "--id", id, "--no-sync", "true")
	if r.code != 124 || !strings.Contains(r.stderr, "error:") {
		t.Errorf("gone session: %+v", r)
	}
}

func TestRunRefusesAnOversizeWorkspaceBeforeTouchingTheBox(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	h.max = 10
	dir := workspace(t, map[string]string{"big.txt": strings.Repeat("x", 100)})
	r := h.run("run", "--json", "--id", id, "--dir", dir, "true")
	if r.code != 121 {
		t.Fatalf("%+v", r)
	}
	if e := find(r.events(t), "error"); e == nil || e["kind"] != "sync_too_large" {
		t.Errorf("error event = %v", e)
	}
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "Sync") || strings.HasPrefix(c, "Exec") {
			t.Fatalf("a request followed a refused sync: %q", f.Calls())
		}
	}
}

func TestEnvForwardingIsExplicitNamedAndNeverPrinted(t *testing.T) {
	const secret = "s3cr3t-value-0b1c"
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	h.env["API_KEY"] = secret
	h.env["OTHER"] = "unforwarded"

	r := h.run("run", "--json", "--id", id, "--no-sync", "--env", "API_KEY", "printenv")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := f.LastExec().Env; !reflect.DeepEqual(got, map[string]string{"API_KEY": secret}) {
		t.Fatalf("forwarded env = %v: only the named variable may travel", got)
	}
	// The CLI's own output never carries the value. (The fake program above
	// printed nothing; a program that echoes its environment is the program's
	// choice and is its stdout.)
	if leaks(r, secret) {
		t.Fatalf("the CLI printed an environment value:\n%s\n%s", r.stdout, r.stderr)
	}
	// The same goes for the human mode and for the call log.
	r = h.run("run", "--id", id, "--no-sync", "--env", "API_KEY", "printenv")
	if leaks(r, secret) {
		t.Fatalf("human mode printed an environment value:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, c := range f.Calls() {
		if strings.Contains(c, secret) {
			t.Fatalf("call log: %q", c)
		}
	}
}

// ANTI-VACUITY for the test above: the leak detector must be able to say yes.
func TestTheLeakDetectorCanFire(t *testing.T) {
	if !leaks(result{stdout: "x TOKEN=abc y"}, "abc") || !leaks(result{stderr: "abc"}, "abc") {
		t.Fatal("leaks() cannot detect a leak, so the test using it proves nothing")
	}
	if leaks(result{stdout: "clean"}, "abc") {
		t.Fatal("leaks() fires on clean output")
	}
	// And a program that does echo its environment shows up as its stdout,
	// which proves the value really was delivered to it.
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	h.env["API_KEY"] = "abc"
	f.ExecFunc = func(req transport.ExecRequest, s transport.Streams) int {
		s.Stdout.Write([]byte(req.Env["API_KEY"]))
		return 0
	}
	if r := h.run("run", "--id", id, "--no-sync", "--env", "API_KEY", "x"); r.stdout != "abc" {
		t.Fatalf("the program did not receive the value: %+v", r)
	}
}

func leaks(r result, secret string) bool {
	return strings.Contains(r.stdout, secret) || strings.Contains(r.stderr, secret)
}

func TestEnvForwardingRefusals(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"not set", []string{"--env", "MISSING"}, nil, "MISSING"},
		{"bad name", []string{"--env", "A=B"}, map[string]string{"A=B": "x"}, "not a valid"},
		{"empty name", []string{"--env", ""}, nil, "not a valid"},
		{"github token", []string{"--env", "GITHUB_TOKEN"}, map[string]string{"GITHUB_TOKEN": "t"}, "never forwarded"},
		{"actions var", []string{"--env", "ACTIONS_RUNTIME_TOKEN"}, map[string]string{"ACTIONS_RUNTIME_TOKEN": "t"}, "never forwarded"},
		{"lowercase github", []string{"--env", "github_sha"}, map[string]string{"github_sha": "t"}, "never forwarded"},
		{"our own token", []string{"--env", "RUNNERS_TOKEN"}, map[string]string{"RUNNERS_TOKEN": "t"}, "never forwarded"},
	}
	for _, c := range cases {
		f := fake.New()
		h := newHarness(t, f)
		id := warm(t, h, f)
		for k, v := range c.env {
			h.env[k] = v
		}
		before := len(f.Calls())
		args := append([]string{"run", "--id", id, "--no-sync"}, c.args...)
		r := h.run(append(args, "true")...)
		if r.code != 120 || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%s: %+v (want 120 mentioning %q)", c.name, r, c.want)
		}
		if len(f.Calls()) != before {
			t.Errorf("%s: a request was made despite the refusal", c.name)
		}
	}
}

func TestARemoteExitInTheReservedRangeIsPassedThroughAndFlagged(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	f.ExecFunc = func(transport.ExecRequest, transport.Streams) int { return 123 }

	r := h.run("run", "--id", id, "--no-sync", "x")
	if r.code != 123 || !strings.Contains(r.stderr, "also a Scratch machine status code") {
		t.Errorf("human: %+v", r)
	}
	r = h.run("run", "--json", "--id", id, "--no-sync", "x")
	if r.code != 123 {
		t.Fatalf("%+v", r)
	}
	ex := find(r.events(t), "exit")
	// The exit event, unlike the process status, is unambiguous: remote true
	// says the command produced the code, and an error event would say the
	// platform did.
	if ex["code"] != float64(123) || ex["remote"] != true || find(r.events(t), "error") != nil {
		t.Errorf("events = %v", r.events(t))
	}
	if find(r.events(t), "warning") == nil {
		t.Error("no warning event for an ambiguous exit code")
	}
}

func TestSSHPassesArgsAndTheSessionExitCode(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	f.ShellCode = 5
	r := h.run("ssh", "--id", id, "--", "-t", "bash")
	if r.code != 5 {
		t.Fatalf("%+v", r)
	}
	if got := f.Calls()[1]; got != `Shell `+id+` args=["-t" "bash"]` {
		t.Errorf("call = %q", got)
	}
	if r := h.run("ssh", "--id", "sm-nope"); r.code != 124 {
		t.Errorf("gone: %+v", r)
	}
}

func TestStopBillsAndASecondStopIsSessionLost(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	f.Advance(2*time.Minute + 3*time.Second)

	r := h.run("stop", "--json", "--id", id)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	evs := r.events(t)
	if !reflect.DeepEqual(names(evs), []string{"stopped", "billed"}) || evs[1]["seconds"] != float64(123) || evs[0]["id"] != id {
		t.Fatalf("events = %v", evs)
	}

	f2 := fake.New()
	h2 := newHarness(t, f2)
	id2 := warm(t, h2, f2)
	r = h2.run("stop", "--id", id2)
	if r.code != 0 || !strings.Contains(r.stderr, "billed 0s") {
		t.Fatalf("human: %+v", r)
	}
	if r := h2.run("stop", "--id", id2); r.code != 124 {
		t.Errorf("second stop: %+v", r)
	}
}

func TestStatus(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	r := h.run("status", "--json", "--id", id)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	st := find(r.events(t), "status")
	if st["id"] != id || st["state"] != "ready" || st["size"] != float64(2) ||
		st["idle_deadline"] != "2026-10-31T12:10:00Z" || st["hard_deadline"] != "2026-10-31T16:00:00Z" {
		t.Errorf("status = %v", st)
	}
	if r := h.run("status", "--id", id); r.code != 0 || !strings.Contains(r.stdout+r.stderr, "ready") {
		t.Errorf("human: %+v", r)
	}
}

// With no service behind it every command says so, in the "not ready" code,
// and does not pretend to have done anything.
func TestTheUnavailableTransportRefusesEveryCommand(t *testing.T) {
	h := newHarness(t, transport.Unavailable{})
	dir := workspace(t, map[string]string{"a": "1"})
	for _, args := range [][]string{
		{"warmup"},
		{"run", "--id", "sm-1", "--dir", dir, "true"},
		{"run", "--id", "sm-1", "--no-sync", "true"},
		{"ssh", "--id", "sm-1"},
		{"stop", "--id", "sm-1"},
		{"status", "--id", "sm-1"},
		{"sync", "--id", "sm-1", "--dir", dir},
		{"pull", "--id", "sm-1", "out"},
		{"list"},
		{"extend", "--id", "sm-1"},
		{"proxy", "--id", "sm-1"},
		{"login"},
	} {
		r := h.run(args...)
		if r.code != platform.KindNotReady.ExitCode() || r.stdout != "" || !strings.Contains(r.stderr, "not available yet") {
			t.Errorf("%v: %+v", args, r)
		}
	}
}

func TestEveryCommandRefusesAMissingID(t *testing.T) {
	h := newHarness(t, fake.New())
	for _, c := range []string{"run", "ssh", "stop", "status", "sync", "pull", "extend", "proxy"} {
		args := []string{c}
		if c == "run" {
			args = append(args, "true")
		}
		if c == "pull" {
			args = append(args, "out")
		}
		if r := h.run(args...); r.code != 120 || !strings.Contains(r.stderr, "--id") {
			t.Errorf("%s: %+v", c, r)
		}
	}
}
