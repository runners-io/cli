// Package transport is the seam between the command line and the Scratch
// machine service.
//
// The CLI decides what to ask (a size, a command, which files); a Transport
// carries the request out. Today there is exactly one real Transport, Unavailable,
// which refuses everything: the service does not exist yet, and a CLI that
// quietly did nothing would read as one that worked. The fake in
// transport/fake is what every test, and the development loop for the
// real transport, runs against.
//
// A Transport reports a refusal as a *platform.Error so the CLI can map it to
// an exit code; anything else is treated as an internal failure.
package transport

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"

	"github.com/runners-io/cli/internal/manifest"
	"github.com/runners-io/cli/internal/platform"
)

// WarmupRequest asks for a box.
type WarmupRequest struct {
	Size        int // vCPUs
	Image       string
	IdleTimeout time.Duration
	MaxDuration time.Duration
}

// Box is a ready box.
type Box struct {
	ID          string
	Size        int
	Image       string
	IdleTimeout time.Duration
	MaxDuration time.Duration
}

// SyncResult is what the box now holds of the sent workspace.
type SyncResult struct {
	Files int
	Bytes int64
}

// ExecRequest runs one command in the synced workspace.
type ExecRequest struct {
	Argv []string
	// Env holds the variables forwarded from the laptop with --env. The values
	// are secrets until proven otherwise: a Transport must not log them and
	// the CLI never prints them.
	Env map[string]string
}

// SetupRequest runs a customer's setup script once, on a machine that has just
// booted and has not yet been announced as ready (`warmup --setup`).
type SetupRequest struct {
	// Script is the file's bytes, run by the machine's shell as the same user
	// a command runs as. It is not interpreted here.
	Script []byte
	// Env holds the variables forwarded from the laptop with --env, for this
	// script only. Same rule as ExecRequest.Env: secrets until proven
	// otherwise, never logged, never stored on our side.
	Env map[string]string
}

// Detached is a command the machine's supervisor is now running without a
// connection from this tool (`run --detach`).
type Detached struct {
	// CommandID names the command on that machine.
	CommandID string
	// Log is the path, on the machine, where the supervisor keeps the command's
	// output. It is the machine's to choose; the CLI only reports it.
	Log string
}

// Streams receive the command's output as it arrives.
type Streams struct {
	Stdout io.Writer
	Stderr io.Writer
}

// StdIO is an interactive session's terminal side.
type StdIO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Status is a box's state.
type Status struct {
	ID           string
	State        string // "ready" or "stopping"
	Size         int
	Image        string
	ReadyAt      time.Time
	IdleDeadline time.Time
	HardDeadline time.Time
}

// StopResult reports what stopping cost.
type StopResult struct {
	BilledSeconds int64
}

// DeviceCode is the answer to "I want to sign in" (the device-code flow, RFC 8628).
// UserCode and VerificationURL are for a person to read; DeviceCode is the
// secret the CLI polls with and is never printed.
type DeviceCode struct {
	DeviceCode      string
	UserCode        string
	VerificationURL string
	Interval        time.Duration
	ExpiresIn       time.Duration
}

// ErrAuthorizationPending is what DevicePoll returns until the person has
// approved the code; ErrSlowDown asks the CLI to poll less often.
var (
	ErrAuthorizationPending = errors.New("authorization pending")
	ErrSlowDown             = errors.New("slow down")
)

// PullFile is one file a pull delivers. The path comes from the machine and is
// untrusted: the CLI validates it before it touches the disk.
type PullFile struct {
	Path       string      // slash-separated, relative to the pulled root
	Mode       fs.FileMode // type bits set for anything that is not a regular file
	Executable bool
	Data       io.Reader
}

type tokenKey struct{}

// WithToken returns a context carrying the caller's credential. A Transport
// reads it with TokenFrom and sends it as its authorization; it must not log
// it, and the CLI never prints it.
func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey{}, token)
}

// TokenFrom is the credential WithToken stored, or "".
func TokenFrom(ctx context.Context) string {
	t, _ := ctx.Value(tokenKey{}).(string)
	return t
}

// Transport carries the CLI's requests to the service.
type Transport interface {
	// DeviceStart begins a device-code sign-in. It needs no credential.
	DeviceStart(ctx context.Context) (DeviceCode, error)
	// DevicePoll asks whether the code was approved. It returns the personal
	// token once, ErrAuthorizationPending or ErrSlowDown while waiting, or a
	// platform.KindAuth error when the code was refused or expired.
	DevicePoll(ctx context.Context, deviceCode string) (token string, err error)
	// List reads every live box of the signed-in organisation.
	List(ctx context.Context) ([]Status, error)
	// Extend moves the box's idle deadline to now plus by (never past its hard
	// deadline) and returns the box's new state.
	Extend(ctx context.Context, id string, by time.Duration) (Status, error)
	// Pull streams the files under a path of the box's workspace to fn. A
	// non-nil error from fn stops the pull and is returned.
	Pull(ctx context.Context, id, remotePath string, fn func(PullFile) error) error
	// Dial opens the byte pipe to the box's SSH port through the relay. It is
	// what an ssh ProxyCommand connects to; nothing is spoken over it here.
	Dial(ctx context.Context, id string) (io.ReadWriteCloser, error)
	// Warmup boots a box and returns once it is ready, or refuses.
	Warmup(ctx context.Context, req WarmupRequest) (Box, error)
	// Sync makes the box's workspace match the manifest, reading content from src.
	Sync(ctx context.Context, id string, m manifest.Manifest, src fs.FS) (SyncResult, error)
	// Exec runs a command and returns its exit code. A non-zero code is not an
	// error; an error means the command could not be run to completion.
	Exec(ctx context.Context, id string, req ExecRequest, out Streams) (int, error)
	// Setup runs the customer's setup script on a machine that is booted but
	// not yet announced, and returns its exit code. As with Exec, a non-zero
	// code is not an error; an error means the script could not be run to
	// completion.
	Setup(ctx context.Context, id string, req SetupRequest, out Streams) (int, error)
	// Start hands a command to the machine's supervisor and returns as soon as
	// the supervisor has it, without waiting for the command or holding a
	// connection open for it. The command's exit code is not reported here.
	Start(ctx context.Context, id string, req ExecRequest) (Detached, error)
	// Shell opens an interactive session and returns its exit code.
	Shell(ctx context.Context, id string, args []string, stdio StdIO) (int, error)
	// Stop ends the box and reports what it billed.
	Stop(ctx context.Context, id string) (StopResult, error)
	// Status reads the box's state.
	Status(ctx context.Context, id string) (Status, error)
}

// Unavailable is the Transport of a build that has no service to talk to.
type Unavailable struct{}

var _ Transport = Unavailable{}

func (Unavailable) err() error {
	return platform.Errorf(platform.KindNotReady, "the Scratch machine is not available yet")
}

func (u Unavailable) DeviceStart(context.Context) (DeviceCode, error) {
	return DeviceCode{}, u.err()
}
func (u Unavailable) DevicePoll(context.Context, string) (string, error) { return "", u.err() }
func (u Unavailable) List(context.Context) ([]Status, error)             { return nil, u.err() }
func (u Unavailable) Extend(context.Context, string, time.Duration) (Status, error) {
	return Status{}, u.err()
}
func (u Unavailable) Pull(context.Context, string, string, func(PullFile) error) error {
	return u.err()
}
func (u Unavailable) Dial(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, u.err()
}

func (u Unavailable) Warmup(context.Context, WarmupRequest) (Box, error) { return Box{}, u.err() }
func (u Unavailable) Sync(context.Context, string, manifest.Manifest, fs.FS) (SyncResult, error) {
	return SyncResult{}, u.err()
}
func (u Unavailable) Exec(context.Context, string, ExecRequest, Streams) (int, error) {
	return 0, u.err()
}
func (u Unavailable) Setup(context.Context, string, SetupRequest, Streams) (int, error) {
	return 0, u.err()
}
func (u Unavailable) Start(context.Context, string, ExecRequest) (Detached, error) {
	return Detached{}, u.err()
}
func (u Unavailable) Shell(context.Context, string, []string, StdIO) (int, error) { return 0, u.err() }
func (u Unavailable) Stop(context.Context, string) (StopResult, error)            { return StopResult{}, u.err() }
func (u Unavailable) Status(context.Context, string) (Status, error)              { return Status{}, u.err() }
