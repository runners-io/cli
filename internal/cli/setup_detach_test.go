package cli

// Tests for the three flags that were held back until the guest supervisor was
// designed: `warmup --setup`, `warmup --env` and `run --detach` (RUN-2421,
// design section 6). Written before the code, against the in-memory fake.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/runners-io/cli/internal/platform"
	"github.com/runners-io/cli/internal/transport"
	"github.com/runners-io/cli/internal/transport/fake"
)

const setupScript = "#!/bin/sh\napt-get install -y jq\n"

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "setup.sh")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func callNames(f *fake.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		out = append(out, strings.Fields(c)[0])
	}
	return out
}

// ---- warmup --setup ----

func TestWarmupSetupRunsTheScriptBeforeTheMachineIsReady(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	script := writeScript(t, setupScript)
	f.SetupFunc = func(req transport.SetupRequest, s transport.Streams) int {
		s.Stdout.Write([]byte("installing\n"))
		s.Stderr.Write([]byte("note\n"))
		return 0
	}

	r := h.run("warmup", "--setup", script)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	// stdout holds the id and nothing else, even though the script printed.
	if r.stdout != "sm-1\n" {
		t.Errorf("stdout = %q, want only the id", r.stdout)
	}
	if !strings.Contains(r.stderr, "installing") || !strings.Contains(r.stderr, "note") {
		t.Errorf("the script's output should reach stderr in human mode: %q", r.stderr)
	}
	if got := callNames(f); !reflect.DeepEqual(got, []string{"Warmup", "Setup"}) {
		t.Fatalf("calls = %v", got)
	}
	if got := string(f.LastSetup().Script); got != setupScript {
		t.Errorf("the box received %q, want the file's bytes", got)
	}
}

func TestWarmupSetupJSONEventOrder(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	script := writeScript(t, setupScript)
	f.SetupFunc = func(_ transport.SetupRequest, s transport.Streams) int {
		s.Stdout.Write([]byte("hello\n"))
		return 0
	}
	r := h.run("warmup", "--json", "--setup", script)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	evs := r.events(t)
	want := []string{"warmup", "setup", "stdout", "setup_exit", "ready"}
	if got := names(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if evs[0]["setup_bytes"] != float64(len(setupScript)) {
		t.Errorf("warmup event = %v", evs[0])
	}
	if evs[1]["id"] != "sm-1" || evs[3]["code"] != float64(0) || evs[3]["id"] != "sm-1" {
		t.Errorf("setup events = %v %v", evs[1], evs[3])
	}
	if evs[2]["data"] != "hello\n" {
		t.Errorf("stdout event = %v", evs[2])
	}
	for _, e := range evs {
		if e["v"] != float64(1) || e["ts"] == nil {
			t.Errorf("event without the v1 frame: %v", e)
		}
	}
}

func TestWarmupWithoutSetupIsUnchanged(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	r := h.run("warmup", "--json")
	if got := names(r.events(t)); !reflect.DeepEqual(got, []string{"warmup", "ready"}) {
		t.Errorf("events = %v", got)
	}
	if got := callNames(f); !reflect.DeepEqual(got, []string{"Warmup"}) {
		t.Errorf("calls = %v: no setup was asked for", got)
	}
	if w := find(r.events(t), "warmup"); w["setup_bytes"] != nil || w["env"] != nil {
		t.Errorf("the warmup event grew fields nobody asked for: %v", w)
	}
}

func TestWarmupEnvIsForwardedToTheSetupScriptByNameOnly(t *testing.T) {
	const secret = "npm-token-VALUE-4f2a"
	f := fake.New()
	h := newHarness(t, f)
	h.env["NPM_TOKEN"] = secret
	h.env["UNRELATED"] = "stays-home"
	script := writeScript(t, setupScript)

	for _, json := range []bool{true, false} {
		args := []string{"warmup", "--setup", script, "--env", "NPM_TOKEN"}
		if json {
			args = append(args, "--json")
		}
		r := h.run(args...)
		if r.code != 0 {
			t.Fatalf("json=%v: %+v", json, r)
		}
		if got := f.LastSetup().Env; !reflect.DeepEqual(got, map[string]string{"NPM_TOKEN": secret}) {
			t.Fatalf("json=%v: forwarded env = %v: only the named variable may travel", json, got)
		}
		if leaks(r, secret) {
			t.Fatalf("json=%v: the CLI printed an environment value:\n%s\n%s", json, r.stdout, r.stderr)
		}
		if json {
			if w := find(r.events(t), "warmup"); !reflect.DeepEqual(w["env"], []any{"NPM_TOKEN"}) {
				t.Errorf("the warmup event should name the variables, never their values: %v", w)
			}
		}
	}
	for _, c := range f.Calls() {
		if strings.Contains(c, secret) || strings.Contains(c, "stays-home") {
			t.Fatalf("call log: %q", c)
		}
	}
}

func TestWarmupSetupFailureStopsTheMachineAndNeverAnnouncesIt(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	script := writeScript(t, setupScript)
	f.SetupFunc = func(_ transport.SetupRequest, s transport.Streams) int {
		s.Stderr.Write([]byte("E: no such package\n"))
		return 100
	}

	r := h.run("warmup", "--setup", script)
	if r.code != 125 {
		t.Fatalf("exit %d, want 125 (the machine did not become ready): %+v", r.code, r)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q: an id nobody can use must not be printed", r.stdout)
	}
	if !strings.Contains(r.stderr, "setup script exited 100") || !strings.Contains(r.stderr, "stopped") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if ids := f.IDs(); len(ids) != 0 {
		t.Fatalf("the machine %v is still running and billing after a failed setup", ids)
	}

	f2 := fake.New()
	h2 := newHarness(t, f2)
	f2.SetupFunc = func(transport.SetupRequest, transport.Streams) int { return 100 }
	r = h2.run("warmup", "--json", "--setup", script)
	evs := r.events(t)
	want := []string{"warmup", "setup", "setup_exit", "stopped", "billed", "error"}
	if r.code != 125 || !reflect.DeepEqual(names(evs), want) {
		t.Fatalf("code %d events %v, want %v", r.code, names(evs), want)
	}
	if find(evs, "ready") != nil {
		t.Error("a ready event was emitted for a machine whose setup failed")
	}
	if e := find(evs, "setup_exit"); e["code"] != float64(100) {
		t.Errorf("setup_exit = %v: the script's own code belongs here", e)
	}
	if e := find(evs, "error"); e["kind"] != "not_ready" || e["code"] != float64(125) {
		t.Errorf("error = %v", e)
	}
}

func TestWarmupSetupTransportFailureStillStopsTheMachine(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	script := writeScript(t, setupScript)
	f.FailNext("Setup", platformErr())

	r := h.run("warmup", "--setup", script)
	if r.code != 125 {
		t.Fatalf("%+v", r)
	}
	if ids := f.IDs(); len(ids) != 0 {
		t.Fatalf("machine %v left running", ids)
	}
	if got := callNames(f); !reflect.DeepEqual(got, []string{"Warmup", "Setup", "Stop"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestWarmupSetupFailureThatCannotStopTheMachineSaysWhichOne(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	script := writeScript(t, setupScript)
	f.SetupFunc = func(transport.SetupRequest, transport.Streams) int { return 1 }
	f.FailNext("Stop", platformErr())

	r := h.run("warmup", "--setup", script)
	if r.code != 125 || !strings.Contains(r.stderr, "sm-1") || !strings.Contains(r.stderr, "runners scratch stop") {
		t.Errorf("a machine that may still be billing must be named with the way to stop it: %+v", r)
	}
}

func TestWarmupSetupAndEnvRefuseBeforeAnyRequest(t *testing.T) {
	big := strings.Repeat("x", 256<<10+1)
	dir := t.TempDir()
	empty := writeScript(t, "")
	good := writeScript(t, setupScript)
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"env without setup", []string{"--env", "NPM_TOKEN"}, map[string]string{"NPM_TOKEN": "t"}, "--setup"},
		{"missing script", []string{"--setup", filepath.Join(dir, "nope.sh")}, nil, "nope.sh"},
		{"a directory", []string{"--setup", dir}, nil, "regular file"},
		{"empty script", []string{"--setup", empty}, nil, "empty"},
		{"oversize script", []string{"--setup", writeScript(t, big)}, nil, "too large"},
		{"unset variable", []string{"--setup", good, "--env", "MISSING"}, nil, "MISSING"},
		{"github token", []string{"--setup", good, "--env", "GITHUB_TOKEN"}, map[string]string{"GITHUB_TOKEN": "t"}, "never forwarded"},
		{"our own token", []string{"--setup", good, "--env", "RUNNERS_TOKEN"}, map[string]string{"RUNNERS_TOKEN": "t"}, "never forwarded"},
		{"actions oidc", []string{"--setup", good, "--env", "ACTIONS_ID_TOKEN_REQUEST_TOKEN"}, map[string]string{"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "t"}, "never forwarded"},
		{"other oidc", []string{"--setup", good, "--env", "CLOUD_OIDC_TOKEN"}, map[string]string{"CLOUD_OIDC_TOKEN": "t"}, "never forwarded"},
		{"id token", []string{"--setup", good, "--env", "GOOGLE_ID_TOKEN"}, map[string]string{"GOOGLE_ID_TOKEN": "t"}, "never forwarded"},
		{"setup takes a path argument", []string{"--setup"}, nil, "setup"},
	}
	for _, c := range cases {
		f := fake.New()
		h := newHarness(t, f)
		for k, v := range c.env {
			h.env[k] = v
		}
		r := h.run(append([]string{"warmup"}, c.args...)...)
		if r.code != 120 || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%s: %+v (want 120 mentioning %q)", c.name, r, c.want)
		}
		if n := len(f.Calls()); n != 0 {
			t.Errorf("%s: %d request(s) were made despite the refusal: %v", c.name, n, f.Calls())
		}
	}
}

// --env is a NAME, never NAME=value: a value typed on a command line is in the
// shell history and the process list, and an error message must not repeat it.
func TestEnvTakesAKeyOnlyAndAnErrorNeverEchoesAValueTypedAfterIt(t *testing.T) {
	const secret = "hunter2-typed-by-mistake"
	script := writeScript(t, setupScript)
	for _, args := range [][]string{
		{"warmup", "--setup", script, "--env", "API_KEY=" + secret},
		{"run", "--id", "sm-1", "--no-sync", "--env", "API_KEY=" + secret, "true"},
		{"run", "--detach", "--id", "sm-1", "--no-sync", "--env", "API_KEY=" + secret, "true"},
	} {
		f := fake.New()
		h := newHarness(t, f)
		r := h.run(args...)
		if r.code != 120 || !strings.Contains(r.stderr, "not a valid") {
			t.Errorf("%v: %+v", args, r)
		}
		if leaks(r, secret) {
			t.Errorf("%v: the refusal printed the value:\n%s", args, r.stderr)
		}
		if len(f.Calls()) != 0 {
			t.Errorf("%v: a request was made", args)
		}
	}
}

// ---- run --detach ----

func TestRunDetachSyncsStartsAndReturnsWithoutWaiting(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	dir := workspace(t, map[string]string{"main.go": "package main"})
	f.ExecFunc = func(transport.ExecRequest, transport.Streams) int {
		t.Error("a detached run waited for the command (Exec was called)")
		return 0
	}

	r := h.run("run", "--detach", "--id", id, "--dir", dir, "--", "make", "bench")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if r.stdout != "cmd-1\n" {
		t.Errorf("stdout = %q, want only the command id", r.stdout)
	}
	if !strings.Contains(r.stderr, "/tmp/scratch/cmd-1.log") {
		t.Errorf("stderr should say where the output is kept: %q", r.stderr)
	}
	calls := f.Calls()
	if len(calls) != 3 || !strings.HasPrefix(calls[1], "Sync "+id) || !strings.HasPrefix(calls[2], "Start "+id) {
		t.Fatalf("calls = %q", calls)
	}
	if got := f.LastStart().Argv; !reflect.DeepEqual(got, []string{"make", "bench"}) {
		t.Errorf("argv = %v", got)
	}
}

func TestRunDetachJSONEvents(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	dir := workspace(t, map[string]string{"a": "1"})

	r := h.run("run", "--detach", "--json", "--id", id, "--dir", dir, "--", "sleep", "600")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	evs := r.events(t)
	want := []string{"syncing", "detached", "warning"}
	if got := names(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	d := find(evs, "detached")
	if d["id"] != id || d["command"] != "cmd-1" || d["log"] != "/tmp/scratch/cmd-1.log" {
		t.Errorf("detached = %v", d)
	}
	// There is no exit event: the command has not exited, and the process
	// status of a detached run is the CLI's, never the command's.
	if find(evs, "exit") != nil {
		t.Error("a detached run emitted an exit event")
	}
	if w := find(evs, "warning"); !strings.Contains(w["message"].(string), "idle") {
		t.Errorf("the warning should say the idle clock still runs: %v", w)
	}
}

func TestRunDetachNoSyncAndEnvByNameOnly(t *testing.T) {
	const secret = "detached-secret-VALUE-77"
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	h.env["API_KEY"] = secret
	h.env["OTHER"] = "unforwarded"

	for _, json := range []bool{true, false} {
		args := []string{"run", "--detach", "--no-sync", "--id", id, "--env", "API_KEY"}
		if json {
			args = append(args, "--json")
		}
		r := h.run(append(args, "--", "./serve")...)
		if r.code != 0 {
			t.Fatalf("json=%v: %+v", json, r)
		}
		if got := f.LastStart().Env; !reflect.DeepEqual(got, map[string]string{"API_KEY": secret}) {
			t.Fatalf("json=%v: forwarded env = %v", json, got)
		}
		if leaks(r, secret) {
			t.Fatalf("json=%v: the CLI printed an environment value", json)
		}
	}
	for _, n := range callNames(f) {
		if n == "Sync" {
			t.Error("--no-sync still synced")
		}
	}
	for _, c := range f.Calls() {
		if strings.Contains(c, secret) {
			t.Fatalf("call log: %q", c)
		}
	}
}

func TestRunDetachRefusalsAndPlatformCodes(t *testing.T) {
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	dir := workspace(t, map[string]string{"a": "1"})
	before := len(f.Calls())

	if r := h.run("run", "--detach", "--id", id, "--dir", dir); r.code != 120 || !strings.Contains(r.stderr, "needs a command") {
		t.Errorf("no command: %+v", r)
	}
	if r := h.run("run", "--detach", "--id", id, "--env", "NOT_SET", "--", "true"); r.code != 120 {
		t.Errorf("unset env: %+v", r)
	}
	if len(f.Calls()) != before {
		t.Errorf("a request was made despite the refusals: %v", f.Calls()[before:])
	}

	// A machine that is gone is 124, and a refused sync stops before Start.
	if r := h.run("run", "--detach", "--id", "sm-9", "--no-sync", "--", "true"); r.code != 124 {
		t.Errorf("gone machine: %+v", r)
	}
	big := workspace(t, map[string]string{"big": strings.Repeat("x", 100)})
	h.max = 10
	if r := h.run("run", "--detach", "--id", id, "--dir", big, "--", "true"); r.code != 121 {
		t.Errorf("oversize: %+v", r)
	}
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "Start "+id) {
			t.Errorf("Start was called after a refused sync: %v", f.Calls())
		}
	}

	// Signed out: stops locally with 123, before any request.
	delete(h.env, "RUNNERS_TOKEN")
	n := len(f.Calls())
	if r := h.run("run", "--detach", "--id", id, "--no-sync", "--", "true"); r.code != 123 || len(f.Calls()) != n {
		t.Errorf("signed out: %+v", r)
	}
}

func TestDetachedCommandIdIsNotARemoteExitCode(t *testing.T) {
	// A detached run that starts is 0 whatever the command later does; the
	// reserved-range warning of a foreground run does not apply.
	f := fake.New()
	h := newHarness(t, f)
	id := warm(t, h, f)
	r := h.run("run", "--detach", "--no-sync", "--id", id, "--", "exit", "124")
	if r.code != 0 || strings.Contains(r.stderr, "also a Scratch machine status code") {
		t.Errorf("%+v", r)
	}
}

func platformErr() error {
	return platform.Errorf(platform.KindNotReady, "the machine went away")
}

func TestTheUnavailableTransportRefusesSetupAndDetach(t *testing.T) {
	h := newHarness(t, transport.Unavailable{})
	script := writeScript(t, setupScript)
	for _, args := range [][]string{
		{"warmup", "--setup", script},
		{"run", "--detach", "--id", "sm-1", "--no-sync", "--", "true"},
	} {
		r := h.run(args...)
		if r.code != platform.KindNotReady.ExitCode() || r.stdout != "" || !strings.Contains(r.stderr, "not available yet") {
			t.Errorf("%v: %+v", args, r)
		}
	}
}

// The stop after a failed setup must still happen when the person has pressed
// Ctrl-C: it runs on a context that is no longer cancelled.
func TestFailedSetupStillStopsTheMachineWhenTheCommandWasInterrupted(t *testing.T) {
	f := fake.New()
	script := writeScript(t, setupScript)
	ctx, cancel := context.WithCancel(context.Background())
	f.SetupFunc = func(transport.SetupRequest, transport.Streams) int {
		cancel() // interrupted while the script runs
		return 130
	}
	var out, errb bytes.Buffer
	code := Run(ctx, Env{
		Args:   []string{"scratch", "warmup", "--setup", script},
		Stdout: &out, Stderr: &errb,
		LookupEnv: func(k string) (string, bool) { return testToken, k == "RUNNERS_TOKEN" },
		Transport: f,
	})
	if code != 125 {
		t.Errorf("code %d: %s", code, errb.String())
	}
	if ids := f.IDs(); len(ids) != 0 {
		t.Fatalf("machine %v left running after an interrupted setup", ids)
	}
}
