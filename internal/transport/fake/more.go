package fake

// The parts of the fake that arrived with sign-in, list, extend, pull and the
// relay pipe (RUN-2421).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runners-io/cli/internal/platform"
	"github.com/runners-io/cli/internal/transport"
)

// DeviceUserCode is the code every fake sign-in shows.
const DeviceUserCode = "WDJB-MJHT"

// DeviceToken is the token a fake sign-in ends with. Tests look for it in
// every output to prove it is never printed.
const DeviceToken = "rst_fake_personal_token_0123456789"

// DeviceSecret is the polling secret of a fake sign-in; it is never printed
// either.
const DeviceSecret = "dev-secret-code-9876543210"

// SetPendingPolls makes the next sign-in answer "pending" n times before it
// approves.
func (f *Fake) SetPendingPolls(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingPolls = n
}

// SetRequiredToken makes every call except the sign-in pair refuse a request
// whose credential is not exactly tok.
func (f *Fake) SetRequiredToken(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requiredToken = tok
}

// Tokens is the credential each authenticated call carried, in call order. It
// exists so a test can prove which one reached the transport.
func (f *Fake) Tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

// SetRemoteFiles sets what a Pull delivers: the file records (Path, Mode,
// Executable) and their contents by path.
func (f *Fake) SetRemoteFiles(files []transport.PullFile, data map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pull = files
	f.pullData = data
}

func (f *Fake) DeviceStart(ctx context.Context) (transport.DeviceCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "DeviceStart", ""); err != nil {
		return transport.DeviceCode{}, err
	}
	return transport.DeviceCode{
		DeviceCode: DeviceSecret, UserCode: DeviceUserCode,
		VerificationURL: "https://example.test/device",
		Interval:        5 * time.Second, ExpiresIn: 15 * time.Minute,
	}, nil
}

func (f *Fake) DevicePoll(ctx context.Context, code string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "DevicePoll", ""); err != nil {
		return "", err
	}
	if code != DeviceSecret {
		return "", platform.Errorf(platform.KindAuth, "the sign-in code was refused")
	}
	if f.pendingPolls > 0 {
		f.pendingPolls--
		return "", transport.ErrAuthorizationPending
	}
	return DeviceToken, nil
}

func (f *Fake) List(ctx context.Context) ([]transport.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "List", ""); err != nil {
		return nil, err
	}
	var ids []string
	for id := range f.boxes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []transport.Status
	for _, id := range ids {
		out = append(out, f.statusOf(f.boxes[id]))
	}
	return out, nil
}

func (f *Fake) Extend(ctx context.Context, id string, by time.Duration) (transport.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Extend", fmt.Sprintf("%s by=%s", id, by)); err != nil {
		return transport.Status{}, err
	}
	b, err := f.lookup(id)
	if err != nil {
		return transport.Status{}, err
	}
	until := f.now.Add(by)
	if hard := b.readyAt.Add(b.MaxDuration); until.After(hard) {
		until = hard
	}
	b.idleUntil = until
	return f.statusOf(b), nil
}

func (f *Fake) Pull(ctx context.Context, id, remote string, fn func(transport.PullFile) error) error {
	f.mu.Lock()
	err := f.enter(ctx, "Pull", id+" "+remote)
	if err == nil {
		_, err = f.lookup(id)
	}
	files, data := f.pull, f.pullData
	f.mu.Unlock()
	if err != nil {
		return err
	}
	for _, pf := range files {
		pf.Data = strings.NewReader(data[pf.Path])
		if err := fn(pf); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fake) Dial(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Dial", id); err != nil {
		return nil, err
	}
	if _, err := f.lookup(id); err != nil {
		return nil, err
	}
	f.relay = &Pipe{Reply: f.RelayReply}
	return f.relay, nil
}

// Relay is the pipe the last Dial returned.
func (f *Fake) Relay() *Pipe {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.relay
}

// Pipe is an in-memory relay end: what the CLI writes lands in Sent, and
// Reply is what the box says back.
type Pipe struct {
	mu     sync.Mutex
	sent   bytes.Buffer
	Reply  string
	rd     *strings.Reader
	closed bool
	wrote  chan struct{}
	once   sync.Once
}

// ready is closed by the first Write.
func (p *Pipe) ready() chan struct{} {
	p.once.Do(func() { p.wrote = make(chan struct{}) })
	return p.wrote
}

// Read answers only after the client has written something (a real box speaks
// after it is spoken to), or after a second, so a test that never writes does
// not hang.
func (p *Pipe) Read(b []byte) (int, error) {
	select {
	case <-p.ready():
	case <-time.After(time.Second):
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rd == nil {
		p.rd = strings.NewReader(p.Reply)
	}
	return p.rd.Read(b)
}

func (p *Pipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	n, err := p.sent.Write(b)
	p.mu.Unlock()
	if len(b) > 0 {
		select {
		case <-p.ready():
		default:
			close(p.ready())
		}
	}
	return n, err
}

func (p *Pipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

// Sent is everything written to the pipe.
func (p *Pipe) Sent() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent.String()
}

// Closed reports whether the pipe was closed.
func (p *Pipe) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (f *Fake) statusOf(b *box) transport.Status {
	idle := b.readyAt.Add(b.IdleTimeout)
	if !b.idleUntil.IsZero() {
		idle = b.idleUntil
	}
	return transport.Status{
		ID: b.ID, State: "ready", Size: b.Size, Image: b.Image, ReadyAt: b.readyAt,
		IdleDeadline: idle, HardDeadline: b.readyAt.Add(b.MaxDuration),
	}
}
