package cli

// The commands that arrived with RUN-2421: login, logout, sync, pull, list,
// extend and the ssh ProxyCommand.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/runners-io/cli/internal/credentials"
	"github.com/runners-io/cli/internal/events"
	"github.com/runners-io/cli/internal/manifest"
	"github.com/runners-io/cli/internal/platform"
	"github.com/runners-io/cli/internal/sshsync"
	"github.com/runners-io/cli/internal/transport"
)

// tokenEnvVar is the service token for agents and CI. It wins over the stored
// personal token, so a script is never silently running as the person.
const tokenEnvVar = "RUNNERS_TOKEN"

// token finds the credential to send: the environment first, then the file.
// The error names where to get one and never the token.
func (s *session) token() (string, error) {
	if v, ok := s.env.LookupEnv(tokenEnvVar); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), nil
	}
	if s.env.Credentials != nil {
		tok, err := s.env.Credentials.Load()
		if err == nil {
			return tok, nil
		}
		if !errors.Is(err, credentials.ErrNone) {
			return "", platform.Errorf(platform.KindAuth, "%v", err)
		}
	}
	return "", platform.Errorf(platform.KindAuth, "not signed in: run `runners scratch login`, or set %s", tokenEnvVar)
}

func (s *session) sleep(ctx context.Context, d time.Duration) error {
	if s.env.Sleep != nil {
		return s.env.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- login / logout ----

const (
	defaultPollInterval = 5 * time.Second
	slowDownBy          = 5 * time.Second
)

func cmdLogin(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("login")
	_ = fs.Bool("json", false, "events as NDJSON on stdout")
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, usageErr("login takes no arguments, got %q", fs.Arg(0))
	}
	if s.env.Credentials == nil {
		return 0, usageErr("there is nowhere to keep a credential: set HOME or XDG_CONFIG_HOME")
	}
	dc, err := s.env.Transport.DeviceStart(ctx)
	if err != nil {
		return 0, err
	}
	// The user code and the address are for a person. The device code is the
	// polling secret and is never printed.
	s.em.Emit(events.Event{Name: "login", Fields: events.Fields{
		"user_code": dc.UserCode, "url": dc.VerificationURL, "expires_in": dc.ExpiresIn.String(),
	}})
	interval := dc.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	deadline := s.env.Now().Add(dc.ExpiresIn)
	for {
		if err := s.sleep(ctx, interval); err != nil {
			return 0, err
		}
		tok, err := s.env.Transport.DevicePoll(ctx, dc.DeviceCode)
		switch {
		case err == nil:
			if err := s.env.Credentials.Save(tok); err != nil {
				return 0, fmt.Errorf("signed in, but the credential could not be stored: %w", err)
			}
			s.em.Emit(events.Event{Name: "signed_in", Fields: events.Fields{}})
			if v, ok := s.env.LookupEnv(tokenEnvVar); ok && v != "" {
				s.em.Emit(events.Event{Name: "warning", Fields: events.Fields{
					"message": tokenEnvVar + " is set and takes precedence over the credential just stored",
				}})
			}
			return 0, nil
		case errors.Is(err, transport.ErrAuthorizationPending):
		case errors.Is(err, transport.ErrSlowDown):
			interval += slowDownBy
		default:
			return 0, err
		}
		if !s.env.Now().Before(deadline) {
			return 0, platform.Errorf(platform.KindAuth, "the sign-in code expired before it was approved; run login again")
		}
	}
}

func cmdLogout(_ context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("logout")
	_ = fs.Bool("json", false, "events as NDJSON on stdout")
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, usageErr("logout takes no arguments, got %q", fs.Arg(0))
	}
	if s.env.Credentials != nil {
		if err := s.env.Credentials.Delete(); err != nil {
			return 0, err
		}
	}
	s.em.Emit(events.Event{Name: "signed_out", Fields: events.Fields{}})
	return 0, nil
}

// ---- sync ----

func cmdSync(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("sync")
	var (
		id    = fs.String("id", "", "machine id (or "+idEnvVar+")")
		dir   = fs.String("dir", ".", "working directory to sync")
		allow stringList
		_     = fs.Bool("json", false, "events as NDJSON on stdout")
	)
	fs.Var(&allow, "allow", "send this secret-shaped file anyway (repeatable, relative to --dir)")
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, usageErr("sync takes no arguments, got %q", fs.Arg(0))
	}
	machine, err := s.resolveID(*id)
	if err != nil {
		return 0, err
	}
	allowPaths, err := cleanAllow(allow)
	if err != nil {
		return 0, err
	}
	return 0, s.sync(ctx, machine, *dir, allowPaths)
}

// ---- list ----

func statusFields(st transport.Status) events.Fields {
	return events.Fields{
		"id": st.ID, "state": st.State, "size": st.Size, "image": st.Image,
		"ready_at":      st.ReadyAt.UTC().Format(time.RFC3339),
		"idle_deadline": st.IdleDeadline.UTC().Format(time.RFC3339),
		"hard_deadline": st.HardDeadline.UTC().Format(time.RFC3339),
	}
}

func cmdList(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("list")
	_ = fs.Bool("json", false, "events as NDJSON on stdout")
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, usageErr("list takes no arguments, got %q", fs.Arg(0))
	}
	boxes, err := s.env.Transport.List(ctx)
	if err != nil {
		return 0, err
	}
	for _, st := range boxes {
		if s.json {
			s.em.Emit(events.Event{Name: "status", Fields: statusFields(st)})
			continue
		}
		fmt.Fprintf(s.env.Stdout, "%s\t%s\t%d vCPU\t%s\tidle deadline %s\n",
			st.ID, st.State, st.Size, st.Image, st.IdleDeadline.UTC().Format(time.RFC3339))
	}
	if len(boxes) == 0 && !s.json {
		fmt.Fprintln(s.env.Stderr, "no running machines")
	}
	return 0, nil
}

// ---- extend ----

const minExtend = time.Minute

func cmdExtend(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("extend")
	var (
		id = fs.String("id", "", "machine id (or "+idEnvVar+")")
		by = fs.Duration("by", 10*time.Minute, "keep the machine alive this long from now (1m-60m); the hard maximum still applies")
		_  = fs.Bool("json", false, "events as NDJSON on stdout")
	)
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, usageErr("extend takes no arguments, got %q", fs.Arg(0))
	}
	if *by < minExtend || *by > maxIdleTimeout {
		return 0, usageErr("--by %s: it must be between %s and %s", *by, minExtend, maxIdleTimeout)
	}
	machine, err := s.resolveID(*id)
	if err != nil {
		return 0, err
	}
	st, err := s.env.Transport.Extend(ctx, machine, *by)
	if err != nil {
		return 0, err
	}
	s.em.Emit(events.Event{Name: "extended", Fields: statusFields(st)})
	if until := st.IdleDeadline; !until.Before(st.HardDeadline) {
		s.em.Emit(events.Event{Name: "warning", Fields: events.Fields{
			"message": "the machine reaches its hard maximum at " + st.HardDeadline.UTC().Format(time.RFC3339) + " whatever you do",
		}})
	}
	return 0, nil
}

// ---- pull ----

func cmdPull(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("pull")
	var (
		id  = fs.String("id", "", "machine id (or "+idEnvVar+")")
		dir = fs.String("dir", ".", "local directory to write into")
		_   = fs.Bool("json", false, "events as NDJSON on stdout")
	)
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	remotes := fs.Args()
	if len(remotes) == 0 {
		return 0, usageErr("pull needs a path in the machine's workspace: runners scratch pull --id ID <path>...")
	}
	for i, r := range remotes {
		p := path.Clean(filepath.ToSlash(r))
		if path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsRune(p, 0) {
			return 0, usageErr("pull %q: give a path inside the workspace, relative to it", r)
		}
		remotes[i] = p
	}
	machine, err := s.resolveID(*id)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return 0, fmt.Errorf("creating %s: %w", *dir, err)
	}
	root, err := os.OpenRoot(*dir)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", *dir, err)
	}
	defer root.Close()

	limit := s.env.MaxWorkspaceBytes
	if limit <= 0 {
		limit = manifest.DefaultMaxWorkspaceBytes
	}
	var files int
	var total int64
	for _, remote := range remotes {
		err := s.env.Transport.Pull(ctx, machine, remote, func(f transport.PullFile) error {
			n, skip, err := writePulled(root, f, limit-total)
			if err != nil {
				return err
			}
			if skip != "" {
				s.em.Emit(events.Event{Name: "skipped", Fields: events.Fields{"path": f.Path, "reason": skip}})
				return nil
			}
			files++
			total += n
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	s.em.Emit(events.Event{Name: "pulled", Fields: events.Fields{"files": files, "bytes": total, "dir": *dir}})
	return 0, nil
}

// writePulled writes one file the machine sent. The machine is untrusted: its
// path is checked, the write goes through an os.Root so no symlink on the way
// can lead outside the directory, only a regular file is written, and the
// result is never setuid, setgid or writable by others. budget is how many more
// bytes the pull may write.
func writePulled(root *os.Root, f transport.PullFile, budget int64) (int64, string, error) {
	if !f.Mode.IsRegular() {
		return 0, "not_regular", nil
	}
	rel := f.Path
	if !fs.ValidPath(rel) || rel == "." || strings.ContainsAny(rel, "\\\x00") {
		return 0, "", usageErr("the machine sent a file with an unsafe path %q; nothing outside the directory was touched", rel)
	}
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			return 0, "", fmt.Errorf("creating the directory for %s: %w", rel, err)
		}
	}
	mode := fs.FileMode(0o644)
	if f.Executable {
		mode = 0o755
	}
	// Write beside the target and rename, so an interrupted pull never leaves a
	// half-written file under a real name.
	tmp := filepath.FromSlash(path.Join(path.Dir(rel), fmt.Sprintf(".scratch-pull-%d", time.Now().UnixNano())))
	out, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, "", fmt.Errorf("writing %s: %w", rel, err)
	}
	n, cerr := io.Copy(out, io.LimitReader(f.Data, budget+1))
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	if cerr == nil && n > budget {
		cerr = platform.Errorf(platform.KindSyncTooLarge, "the pull is over the size limit; stopped at %s", rel)
	}
	if cerr != nil {
		_ = root.Remove(tmp)
		return 0, "", cerr
	}
	if err := root.Rename(tmp, filepath.FromSlash(rel)); err != nil {
		_ = root.Remove(tmp)
		return 0, "", fmt.Errorf("writing %s: %w", rel, err)
	}
	return n, "", nil
}

// ---- proxy ----

// cmdProxy is what ssh runs as its ProxyCommand: it carries ssh's bytes to the
// machine's relay and back. Nothing but those bytes may go to stdout, so the
// human-mode progress lines (stderr) are the only other output.
func cmdProxy(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("proxy")
	id := fs.String("id", "", "machine id")
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, usageErr("proxy takes no arguments, got %q", fs.Arg(0))
	}
	machine, err := s.resolveID(*id)
	if err != nil {
		return 0, err
	}
	if err := sshsync.ValidID(machine); err != nil {
		return 0, usageErr("%v", err)
	}
	conn, err := s.env.Transport.Dial(ctx, machine)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	up := make(chan struct{})
	down := make(chan struct{})
	go func() { _, _ = io.Copy(conn, s.env.Stdin); close(up) }()
	go func() { _, _ = io.Copy(s.env.Stdout, conn); close(down) }()
	select {
	case <-down: // the machine ended the stream
	case <-ctx.Done():
	case <-up: // ssh went away; let any last bytes from the machine out
		select {
		case <-down:
		case <-time.After(time.Second):
		}
	}
	return 0, nil
}
