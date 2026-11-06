package fake

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/runners-io/cli/internal/manifest"
	"github.com/runners-io/cli/internal/platform"
	"github.com/runners-io/cli/internal/transport"
)

var ctx = context.Background()

func wantKind(t *testing.T, err error, want platform.Kind) {
	t.Helper()
	k, ok := platform.KindOf(err)
	if !ok || k != want {
		t.Fatalf("err = %v, want kind %s", err, want)
	}
}

func TestWarmupStatusStopAndBilling(t *testing.T) {
	f := New()
	b, err := f.Warmup(ctx, transport.WarmupRequest{Size: 4, Image: "ubuntu-2404", IdleTimeout: 10 * time.Minute, MaxDuration: 4 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == "" || b.Size != 4 {
		t.Fatalf("box = %+v", b)
	}
	st, err := f.Status(ctx, b.ID)
	if err != nil || st.State != "ready" || st.IdleDeadline.Sub(st.ReadyAt) != 10*time.Minute || st.HardDeadline.Sub(st.ReadyAt) != 4*time.Hour {
		t.Fatalf("status = %+v, %v", st, err)
	}
	f.Advance(95*time.Second + 400*time.Millisecond)
	res, err := f.Stop(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Billed from ready to stop, rounded up to the second: idle minutes are billed (D3).
	if res.BilledSeconds != 96 {
		t.Fatalf("billed %d, want 96", res.BilledSeconds)
	}
	_, err = f.Status(ctx, b.ID)
	wantKind(t, err, platform.KindSessionLost)
	_, err = f.Stop(ctx, b.ID)
	wantKind(t, err, platform.KindSessionLost)
}

func TestMachineCapPerOrganisation(t *testing.T) {
	f := New()
	var ids []string
	for i := 0; i < 3; i++ {
		b, err := f.Warmup(ctx, transport.WarmupRequest{Size: 2})
		if err != nil {
			t.Fatalf("box %d: %v", i, err)
		}
		ids = append(ids, b.ID)
	}
	_, err := f.Warmup(ctx, transport.WarmupRequest{Size: 2})
	wantKind(t, err, platform.KindQuota)
	// Stopping one frees a slot: the cap is a count of live boxes.
	if _, err := f.Stop(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Warmup(ctx, transport.WarmupRequest{Size: 2}); err != nil {
		t.Fatalf("after a stop: %v", err)
	}
}

func TestSyncStoresExactlyWhatTheManifestNamesAndRefusesAnUnknownBox(t *testing.T) {
	f := New()
	b, _ := f.Warmup(ctx, transport.WarmupRequest{Size: 2})
	src := fstest.MapFS{
		"main.go": &fstest.MapFile{Data: []byte("package main"), Mode: 0o644},
		"run.sh":  &fstest.MapFile{Data: []byte("#!/bin/sh"), Mode: 0o755},
		".env":    &fstest.MapFile{Data: []byte("TOKEN=hunter2"), Mode: 0o644},
	}
	m, err := manifest.Build(src, []string{"main.go", "run.sh", ".env"}, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sync(ctx, b.ID, m, src)
	if err != nil || res.Files != 2 || res.Bytes != int64(len("package main")+len("#!/bin/sh")) {
		t.Fatalf("sync = %+v, %v", res, err)
	}
	files := f.Files(b.ID)
	if len(files) != 2 || string(files["main.go"].Data) != "package main" || !files["run.sh"].Executable {
		t.Fatalf("box files = %+v", files)
	}
	if _, ok := files[".env"]; ok {
		t.Fatal("a skipped secret reached the box")
	}
	// A later sync makes the workspace match: files not in the manifest are removed.
	m2, _ := manifest.Build(src, []string{"main.go"}, manifest.Options{})
	if _, err := f.Sync(ctx, b.ID, m2, src); err != nil {
		t.Fatal(err)
	}
	if got := f.Files(b.ID); len(got) != 1 {
		t.Fatalf("after a narrower sync the box holds %v", got)
	}
	_, err = f.Sync(ctx, "sm-nope", m, src)
	wantKind(t, err, platform.KindSessionLost)
}

func TestExecRunsTheHandlerAndPassesEnvAndExitCode(t *testing.T) {
	f := New()
	b, _ := f.Warmup(ctx, transport.WarmupRequest{Size: 2})
	f.ExecFunc = func(req transport.ExecRequest, s transport.Streams) int {
		s.Stdout.Write([]byte(strings.Join(req.Argv, " ") + "\n"))
		s.Stderr.Write([]byte("warn\n"))
		if req.Env["MODE"] != "fast" {
			return 1
		}
		return 7
	}
	var out, errb bytes.Buffer
	code, err := f.Exec(ctx, b.ID, transport.ExecRequest{Argv: []string{"make", "test"}, Env: map[string]string{"MODE": "fast"}}, transport.Streams{Stdout: &out, Stderr: &errb})
	if err != nil || code != 7 {
		t.Fatalf("code, err = %d, %v", code, err)
	}
	if out.String() != "make test\n" || errb.String() != "warn\n" {
		t.Fatalf("out %q err %q", out.String(), errb.String())
	}
}

func TestInjectedFailureFiresOnceThenClears(t *testing.T) {
	f := New()
	f.FailNext("Warmup", platform.Errorf(platform.KindAuth, "token revoked"))
	_, err := f.Warmup(ctx, transport.WarmupRequest{Size: 2})
	wantKind(t, err, platform.KindAuth)
	if _, err := f.Warmup(ctx, transport.WarmupRequest{Size: 2}); err != nil {
		t.Fatalf("the injected failure did not clear: %v", err)
	}
}

func TestExpireEndsTheSession(t *testing.T) {
	f := New()
	b, _ := f.Warmup(ctx, transport.WarmupRequest{Size: 2})
	f.Expire(b.ID)
	_, err := f.Exec(ctx, b.ID, transport.ExecRequest{Argv: []string{"true"}}, transport.Streams{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	wantKind(t, err, platform.KindSessionLost)
}

// The call log is how the CLI's tests check what it asked for. It must not
// carry an environment value, or a test that dumps it would leak one.
func TestCallLogNeverHoldsEnvValues(t *testing.T) {
	f := New()
	b, _ := f.Warmup(ctx, transport.WarmupRequest{Size: 2})
	_, _ = f.Exec(ctx, b.ID, transport.ExecRequest{Argv: []string{"x"}, Env: map[string]string{"K": "sekrit-value"}}, transport.Streams{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	for _, c := range f.Calls() {
		if strings.Contains(c, "sekrit-value") {
			t.Fatalf("call log holds an env value: %q", c)
		}
	}
	if got := f.LastExec(); got.Env["K"] != "sekrit-value" {
		t.Fatalf("LastExec lost the request: %+v", got)
	}
}

func TestShellReturnsConfiguredCode(t *testing.T) {
	f := New()
	b, _ := f.Warmup(ctx, transport.WarmupRequest{Size: 2})
	f.ShellCode = 3
	code, err := f.Shell(ctx, b.ID, []string{"-t"}, transport.StdIO{In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	if err != nil || code != 3 {
		t.Fatalf("code, err = %d, %v", code, err)
	}
}

func TestSetupAndStartObeyTheSameRulesAsExec(t *testing.T) {
	f := New()
	b, err := f.Warmup(ctx, transport.WarmupRequest{Size: 2, Image: "ubuntu-2404", IdleTimeout: time.Minute * 10, MaxDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := f.Setup(ctx, b.ID, transport.SetupRequest{Script: []byte("true"), Env: map[string]string{"K": "secret-v"}},
		transport.Streams{Stdout: &out, Stderr: &out})
	if err != nil || code != 0 {
		t.Fatalf("setup: %d %v", code, err)
	}
	if got := f.LastSetup(); string(got.Script) != "true" || got.Env["K"] != "secret-v" {
		t.Errorf("LastSetup = %+v", got)
	}
	d1, err := f.Start(ctx, b.ID, transport.ExecRequest{Argv: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := f.Start(ctx, b.ID, transport.ExecRequest{Argv: []string{"y"}})
	if d1.CommandID == d2.CommandID || d1.Log == "" || d1.Log == d2.Log {
		t.Errorf("each started command needs its own id and log: %+v %+v", d1, d2)
	}
	for _, c := range f.Calls() {
		if strings.Contains(c, "secret-v") {
			t.Errorf("call log carries a value: %q", c)
		}
	}
	// A box that is gone, and a credential that is refused.
	f.Expire(b.ID)
	_, err = f.Setup(ctx, b.ID, transport.SetupRequest{Script: []byte("true")}, transport.Streams{Stdout: &out, Stderr: &out})
	wantKind(t, err, platform.KindSessionLost)
	_, err = f.Start(ctx, b.ID, transport.ExecRequest{Argv: []string{"x"}})
	wantKind(t, err, platform.KindSessionLost)
	f.SetRequiredToken("good")
	_, err = f.Start(ctx, b.ID, transport.ExecRequest{Argv: []string{"x"}})
	wantKind(t, err, platform.KindAuth)
	_, err = f.Setup(ctx, b.ID, transport.SetupRequest{Script: []byte("true")}, transport.Streams{Stdout: &out, Stderr: &out})
	wantKind(t, err, platform.KindAuth)
}
