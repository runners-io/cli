// Package fake is an in-memory Transport. It is the only thing the CLI's tests
// talk to, and it is written to refuse the way the real service is designed to:
// a cap of three live boxes, a session that is gone after a stop or an expiry,
// billing from ready to stop with idle time counted.
//
// It is a test double. The rules it enforces are the design's (its lifecycle
// and caps section), not a measurement of anything.
package fake

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runners-io/cli/internal/manifest"
	"github.com/runners-io/cli/internal/platform"
	"github.com/runners-io/cli/internal/transport"
)

// File is one file as the box holds it.
type File struct {
	Data       []byte
	Executable bool
}

type box struct {
	transport.Box
	readyAt   time.Time
	idleUntil time.Time // set by Extend; zero means readyAt + IdleTimeout
	files     map[string]File
}

// Fake is the in-memory Transport.
type Fake struct {
	mu    sync.Mutex
	now   time.Time
	next  int
	boxes map[string]*box
	calls []string
	fail  map[string]error
	last  transport.ExecRequest

	lastSetup transport.SetupRequest
	lastStart transport.ExecRequest
	started   int

	pendingPolls  int
	requiredToken string
	tokens        []string
	pull          []transport.PullFile
	pullData      map[string]string
	relay         *Pipe

	// RelayReply is what the next Dial's pipe says back.
	RelayReply string

	// MaxBoxes is the live-box cap. New sets it to 3 (design D5).
	MaxBoxes int
	// ExecFunc runs a command. The default prints "ok" and exits 0.
	ExecFunc func(transport.ExecRequest, transport.Streams) int
	// SetupFunc runs a setup script. The default prints nothing and exits 0.
	SetupFunc func(transport.SetupRequest, transport.Streams) int
	// ShellCode is the exit code Shell returns.
	ShellCode int
}

var _ transport.Transport = (*Fake)(nil)

// New returns a Fake whose clock starts at a fixed instant.
func New() *Fake {
	return &Fake{
		now:      time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
		boxes:    map[string]*box{},
		fail:     map[string]error{},
		MaxBoxes: 3,
		ExecFunc: func(_ transport.ExecRequest, s transport.Streams) int {
			_, _ = s.Stdout.Write([]byte("ok\n"))
			return 0
		},
	}
}

// Now is the fake clock.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake clock.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// FailNext makes the next call of method (Warmup, Sync, Exec, Setup, Start, Shell, Stop,
// Status) return err, once.
func (f *Fake) FailNext(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[method] = err
}

// Expire ends a session without a stop, as the hard max or the idle timeout
// would. Later calls on it answer session-lost.
func (f *Fake) Expire(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.boxes, id)
}

// Calls is the log of method calls, one string each. It names the box and the
// shape of the request, never an environment value.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// LastExec is the last Exec request, environment included.
func (f *Fake) LastExec() transport.ExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// Files is what a box holds.
func (f *Fake) Files(id string) map[string]File {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.boxes[id]
	if !ok {
		return nil
	}
	out := make(map[string]File, len(b.files))
	for k, v := range b.files {
		out[k] = v
	}
	return out
}

// enter logs the call and returns an injected failure if one is queued.
func (f *Fake) enter(ctx context.Context, method, detail string) error {
	f.calls = append(f.calls, method+" "+detail)
	if err := ctx.Err(); err != nil {
		return err // a real transport does not answer a cancelled request
	}
	if err, ok := f.fail[method]; ok {
		delete(f.fail, method)
		return err
	}
	if strings.HasPrefix(method, "Device") {
		return nil // signing in needs no credential
	}
	tok := transport.TokenFrom(ctx)
	f.tokens = append(f.tokens, tok)
	if f.requiredToken != "" && tok != f.requiredToken {
		return platform.Errorf(platform.KindAuth, "the credential was refused")
	}
	return nil
}

func (f *Fake) lookup(id string) (*box, error) {
	b, ok := f.boxes[id]
	if !ok {
		return nil, platform.Errorf(platform.KindSessionLost, "no running Scratch machine %q: it was stopped, expired or never existed", id)
	}
	return b, nil
}

func (f *Fake) Warmup(ctx context.Context, req transport.WarmupRequest) (transport.Box, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Warmup", fmt.Sprintf("size=%d image=%s idle=%s max=%s", req.Size, req.Image, req.IdleTimeout, req.MaxDuration)); err != nil {
		return transport.Box{}, err
	}
	if len(f.boxes) >= f.MaxBoxes {
		return transport.Box{}, platform.Errorf(platform.KindQuota, "the organisation already has %d Scratch machines running", f.MaxBoxes)
	}
	f.next++
	b := &box{
		Box: transport.Box{
			ID: fmt.Sprintf("sm-%d", f.next), Size: req.Size, Image: req.Image,
			IdleTimeout: req.IdleTimeout, MaxDuration: req.MaxDuration,
		},
		readyAt: f.now,
		files:   map[string]File{},
	}
	f.boxes[b.ID] = b
	return b.Box, nil
}

func (f *Fake) Sync(ctx context.Context, id string, m manifest.Manifest, src fs.FS) (transport.SyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Sync", fmt.Sprintf("%s files=%d", id, len(m.Entries))); err != nil {
		return transport.SyncResult{}, err
	}
	b, err := f.lookup(id)
	if err != nil {
		return transport.SyncResult{}, err
	}
	files := map[string]File{}
	var total int64
	for _, e := range m.Entries {
		data, err := fs.ReadFile(src, e.Path)
		if err != nil {
			return transport.SyncResult{}, fmt.Errorf("read %s: %w", e.Path, err)
		}
		files[e.Path] = File{Data: data, Executable: e.Executable}
		total += int64(len(data))
	}
	b.files = files
	return transport.SyncResult{Files: len(files), Bytes: total}, nil
}

func (f *Fake) Exec(ctx context.Context, id string, req transport.ExecRequest, out transport.Streams) (int, error) {
	f.mu.Lock()
	err := f.enter(ctx, "Exec", fmt.Sprintf("%s argv=%q env=%d", id, req.Argv, len(req.Env)))
	if err == nil {
		_, err = f.lookup(id)
	}
	f.last = req
	run := f.ExecFunc
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return run(req, out), nil
}

// LastSetup is the last Setup request, environment included.
func (f *Fake) LastSetup() transport.SetupRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSetup
}

// LastStart is the last Start request, environment included.
func (f *Fake) LastStart() transport.ExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastStart
}

func (f *Fake) Setup(ctx context.Context, id string, req transport.SetupRequest, out transport.Streams) (int, error) {
	f.mu.Lock()
	err := f.enter(ctx, "Setup", fmt.Sprintf("%s bytes=%d env=%d", id, len(req.Script), len(req.Env)))
	if err == nil {
		_, err = f.lookup(id)
	}
	f.lastSetup = req
	run := f.SetupFunc
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if run == nil {
		return 0, nil
	}
	return run(req, out), nil
}

// Start records the command and returns at once; the fake never runs it.
func (f *Fake) Start(ctx context.Context, id string, req transport.ExecRequest) (transport.Detached, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Start", fmt.Sprintf("%s argv=%q env=%d", id, req.Argv, len(req.Env))); err != nil {
		return transport.Detached{}, err
	}
	if _, err := f.lookup(id); err != nil {
		return transport.Detached{}, err
	}
	f.lastStart = req
	f.started++
	cmd := fmt.Sprintf("cmd-%d", f.started)
	return transport.Detached{CommandID: cmd, Log: "/tmp/scratch/" + cmd + ".log"}, nil
}

func (f *Fake) Shell(ctx context.Context, id string, args []string, _ transport.StdIO) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Shell", fmt.Sprintf("%s args=%q", id, args)); err != nil {
		return 0, err
	}
	if _, err := f.lookup(id); err != nil {
		return 0, err
	}
	return f.ShellCode, nil
}

func (f *Fake) Stop(ctx context.Context, id string) (transport.StopResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Stop", id); err != nil {
		return transport.StopResult{}, err
	}
	b, err := f.lookup(id)
	if err != nil {
		return transport.StopResult{}, err
	}
	delete(f.boxes, id)
	// Ready to stop, idle time included, rounded up to a whole second.
	held := f.now.Sub(b.readyAt)
	secs := int64(held / time.Second)
	if held%time.Second != 0 {
		secs++
	}
	return transport.StopResult{BilledSeconds: secs}, nil
}

func (f *Fake) Status(ctx context.Context, id string) (transport.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Status", id); err != nil {
		return transport.Status{}, err
	}
	b, err := f.lookup(id)
	if err != nil {
		return transport.Status{}, err
	}
	return f.statusOf(b), nil
}

// IDs lists the live boxes, sorted.
func (f *Fake) IDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.boxes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
