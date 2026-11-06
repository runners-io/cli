// Package cli is the command line of the Scratch machine tool: the command
// table, flag parsing, validation, and the mapping from what happened to an
// exit code and an event.
//
// It does no network I/O of its own. Every request goes through a
// transport.Transport, which the caller supplies, so the whole of it is tested
// against the in-memory fake and the shipped binary can only ever say "not
// available yet" until a real transport exists.
package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/runners-io/cli/internal/credentials"
	"github.com/runners-io/cli/internal/events"
	"github.com/runners-io/cli/internal/manifest"
	"github.com/runners-io/cli/internal/platform"
	"github.com/runners-io/cli/internal/transport"
)

// Version is set at build time (-ldflags "-X .../cli.Version=...").
var Version = "dev"

// Env is everything Run touches outside itself, so a test can supply all of it.
type Env struct {
	Args      []string // without the program name
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	LookupEnv func(string) (string, bool)
	Now       func() time.Time
	Transport transport.Transport
	// Credentials keeps the token `login` obtains; nil means nowhere to keep one.
	Credentials credentials.Store
	// Sleep waits d or until ctx ends. Nil means a real wait.
	Sleep func(ctx context.Context, d time.Duration) error
	// MaxWorkspaceBytes overrides the sync limit; zero means the default.
	MaxWorkspaceBytes int64
}

// Command is one row of the command table.
type Command struct {
	Name    string
	Summary string
	run     func(context.Context, *session, []string) (int, error)
	// noAuth marks a command that works without a credential.
	noAuth bool
}

// Commands is the command table, in help order.
func Commands() []Command {
	return []Command{
		{Name: "login", Summary: "sign in: print a code to approve in the dashboard", run: cmdLogin, noAuth: true},
		{Name: "logout", Summary: "forget the stored credential", run: cmdLogout, noAuth: true},
		{Name: "warmup", Summary: "boot a machine and print its id", run: cmdWarmup},
		{Name: "sync", Summary: "send the working directory to the machine", run: cmdSync},
		{Name: "run", Summary: "sync the working directory, then run a command in the machine", run: cmdRun},
		{Name: "ssh", Summary: "open an interactive shell in the machine", run: cmdSSH},
		{Name: "pull", Summary: "copy files from the machine's workspace back", run: cmdPull},
		{Name: "list", Summary: "list your running machines", run: cmdList},
		{Name: "extend", Summary: "push the machine's idle deadline out", run: cmdExtend},
		{Name: "stop", Summary: "stop the machine and report what it billed", run: cmdStop},
		{Name: "status", Summary: "show the machine's state and deadlines", run: cmdStatus},
		{Name: "proxy", Summary: "the ssh ProxyCommand: bridge stdin/stdout to the machine (used by sync)", run: cmdProxy},
	}
}

// session is what a command runs with.
type session struct {
	env    Env
	em     events.Emitter
	stdout io.Writer // where the remote program's stdout goes
	stderr io.Writer // where the remote program's stderr goes
	json   bool
}

const idEnvVar = "RUNNERS_SCRATCH_ID"

// Run executes one invocation of `runners` and returns the process exit code.
// The binary has one verb, scratch; everything this tool does is
// `runners scratch <command>` (RUN-2421).
func Run(ctx context.Context, env Env) int {
	if env.Now == nil {
		env.Now = time.Now
	}
	args := env.Args
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, topUsage())
		return platform.KindUsage.ExitCode()
	}
	switch args[0] {
	case "help", "-h", "--help", "-help":
		fmt.Fprint(env.Stdout, topUsage())
		return 0
	case "version", "--version":
		fmt.Fprintf(env.Stdout, "runners %s\n", Version)
		return 0
	case scratchVerb:
		return runScratch(ctx, env, args[1:])
	}
	s := newSession(env, false)
	s.em.Emit(events.Event{Name: "error", Fields: events.Fields{"message": fmt.Sprintf("unknown command %q; run `runners help`", args[0])}})
	return platform.KindUsage.ExitCode()
}

// scratchVerb is the one verb of the runners binary.
const scratchVerb = "scratch"

// runScratch runs one `runners scratch` command; args are what follows the verb.
func runScratch(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage())
		return platform.KindUsage.ExitCode()
	}
	switch args[0] {
	case "help", "-h", "--help", "-help":
		fmt.Fprint(env.Stdout, usage())
		return 0
	}
	for _, c := range Commands() {
		if c.Name != args[0] {
			continue
		}
		s := newSession(env, wantsJSON(args[1:]))
		var code int
		var err error
		if !c.noAuth {
			var tok string
			if tok, err = s.token(); err == nil {
				ctx = transport.WithToken(ctx, tok)
			}
		}
		if err == nil {
			code, err = c.run(ctx, s, args[1:])
		}
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage())
			return 0
		}
		if err != nil {
			code = platform.ExitCodeFor(err)
			fields := events.Fields{"message": err.Error(), "code": code}
			if k, ok := platform.KindOf(err); ok {
				fields["kind"] = k.String()
			} else {
				fields["kind"] = platform.KindUsage.String()
			}
			s.em.Emit(events.Event{Name: "error", Fields: fields})
		}
		return code
	}
	s := newSession(env, false)
	s.em.Emit(events.Event{Name: "error", Fields: events.Fields{"message": fmt.Sprintf("unknown command %q; run `runners scratch help`", args[0])}})
	return platform.KindUsage.ExitCode()
}

// wantsJSON looks for --json before any `--`, for the benefit of an error
// raised while the flags themselves are still being parsed.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--json" || a == "-json" || a == "--json=true" {
			return true
		}
	}
	return false
}

func newSession(env Env, jsonMode bool) *session {
	s := &session{env: env, json: jsonMode}
	if jsonMode {
		j := events.NewJSON(env.Stdout, env.Now)
		s.em = j
		s.stdout = j.Stream("stdout")
		s.stderr = j.Stream("stderr")
	} else {
		s.em = events.NewHuman(env.Stderr)
		s.stdout = env.Stdout
		s.stderr = env.Stderr
	}
	return s
}

// topUsage is the help of the runners binary itself.
func topUsage() string {
	return `Usage: runners <command> [flags]

Commands:
  scratch  a remote machine you sync your working directory to and run commands in

Run ` + "`runners scratch help`" + ` for the scratch commands, the exit codes and the
signing-in rules.
`
}

// usage is the help of `runners scratch`.
func usage() string {
	var b strings.Builder
	b.WriteString("Usage: runners scratch <command> [flags]\n\n")
	b.WriteString("A remote machine you sync your working directory to and run commands in.\n\nCommands:\n")
	for _, c := range Commands() {
		fmt.Fprintf(&b, "  %-8s %s\n", c.Name, c.Summary)
	}
	b.WriteString(`
Every command that acts on one machine takes --id (or RUNNERS_SCRATCH_ID).
Every command except ssh and proxy takes --json: events as one JSON object per
line on stdout. Sign in once with login; scripts set RUNNERS_TOKEN instead.

Exit codes
  0           success
  120         bad command line, or an internal error in this tool
  121         the workspace is too large to sync
  122         quota, cap or billing refused the request
  123         not signed in, or the credential was refused
  124         the machine is gone: stopped, expired or never existed
  125         the machine, or the service, is not ready
  130         interrupted
  other       run: the remote command's own exit code, passed through
              (a command can also exit 120-125; with --json the "exit" event's
              "remote" field tells the two apart)
`)
	return b.String()
}

// ---- flag helpers ----

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return platform.Errorf(platform.KindUsage, "%v", err)
	}
	return nil
}

func usageErr(format string, a ...any) error {
	return platform.Errorf(platform.KindUsage, format, a...)
}

// resolveID prefers the flag, then the environment.
func (s *session) resolveID(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v, ok := s.env.LookupEnv(idEnvVar); ok && v != "" {
		return v, nil
	}
	return "", usageErr("which machine? pass --id or set %s", idEnvVar)
}

// ---- warmup ----

// Limits from the design (decisions D3, D4, D7). The idle-timeout range is
// ASSUMED (design section 3: "settable 2 to 60").
const (
	minIdleTimeout = 2 * time.Minute
	maxIdleTimeout = 60 * time.Minute
	maxMaxDuration = 4 * time.Hour
)

var (
	allowedSizes  = []int{2, 4, 8}
	allowedImages = []string{"ubuntu-2404"}
)

// maxSetupBytes bounds the --setup script. It is a script, not a payload: a
// build that needs more than this should be a file in the workspace.
const maxSetupBytes = 256 << 10

// stopTimeout bounds the clean-up stop after a failed setup, which must still
// happen when the person has interrupted the command.
const stopTimeout = 30 * time.Second

func cmdWarmup(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("warmup")
	var (
		envs      stringList
		size      = fs.Int("size", 2, "vCPUs: 2, 4 or 8")
		img       = fs.String("image", "ubuntu-2404", "machine image")
		idle      = fs.Duration("idle-timeout", 10*time.Minute, "stop after this long with no open connection (2m-60m, billed)")
		maxd      = fs.Duration("max-duration", maxMaxDuration, "stop after this long whatever happens (at most 4h)")
		setupPath = fs.String("setup", "", "run this script once on the machine before it is ready")
		_         = fs.Bool("json", false, "events as NDJSON on stdout")
	)
	fs.Var(&envs, "env", "forward this environment variable, by name, to the --setup script (repeatable)")
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, usageErr("warmup takes no arguments, got %q", fs.Arg(0))
	}
	if !containsInt(allowedSizes, *size) {
		return 0, usageErr("--size %d: a machine has 2, 4 or 8 vCPUs", *size)
	}
	if !containsString(allowedImages, *img) {
		return 0, usageErr("--image %q: the available images are %s", *img, strings.Join(allowedImages, ", "))
	}
	if *idle < minIdleTimeout || *idle > maxIdleTimeout {
		return 0, usageErr("--idle-timeout %s: it must be between %s and %s", *idle, minIdleTimeout, maxIdleTimeout)
	}
	if *maxd <= 0 || *maxd > maxMaxDuration {
		return 0, usageErr("--max-duration %s: it must be longer than zero and at most %s", *maxd, maxMaxDuration)
	}

	var setup *transport.SetupRequest
	if *setupPath == "" {
		if len(envs) > 0 {
			return 0, usageErr("--env on warmup is for the --setup script; without a script there is nothing to give it")
		}
	} else {
		script, err := readSetup(*setupPath)
		if err != nil {
			return 0, err
		}
		fwd, err := resolveEnv(envs, s.env.LookupEnv)
		if err != nil {
			return 0, err
		}
		setup = &transport.SetupRequest{Script: script, Env: fwd}
	}

	warmupFields := events.Fields{
		"size": *size, "image": *img, "idle_timeout": idle.String(), "max_duration": maxd.String(),
	}
	if setup != nil {
		warmupFields["setup_bytes"] = len(setup.Script)
		if len(setup.Env) > 0 {
			// Names only, sorted: a value is never in an event.
			names := make([]string, 0, len(setup.Env))
			for n := range setup.Env {
				names = append(names, n)
			}
			sort.Strings(names)
			warmupFields["env"] = names
		}
	}
	s.em.Emit(events.Event{Name: "warmup", Fields: warmupFields})
	box, err := s.env.Transport.Warmup(ctx, transport.WarmupRequest{
		Size: *size, Image: *img, IdleTimeout: *idle, MaxDuration: *maxd,
	})
	if err != nil {
		return 0, err
	}
	if setup != nil {
		if err := s.runSetup(ctx, box.ID, *setup); err != nil {
			return 0, err
		}
	}
	s.em.Emit(events.Event{Name: "ready", Fields: events.Fields{
		"id": box.ID, "size": box.Size, "image": box.Image,
		"idle_timeout": box.IdleTimeout.String(), "max_duration": box.MaxDuration.String(),
	}})
	if !s.json {
		// The id alone on stdout, so a script can capture it.
		fmt.Fprintln(s.env.Stdout, box.ID)
	}
	return 0, nil
}

// readSetup reads the --setup file. The person named it, so it is not scanned
// for secret-shaped names the way a synced tree is, but it must be a plain,
// non-empty, bounded file.
func readSetup(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, usageErr("--setup: cannot read %s: %v", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, usageErr("--setup: cannot read %s: %v", path, err)
	}
	if !st.Mode().IsRegular() {
		return nil, usageErr("--setup %s: it must be a regular file", path)
	}
	if st.Size() > maxSetupBytes {
		return nil, usageErr("--setup %s: the script is too large (%d bytes; the limit is %d)", path, st.Size(), maxSetupBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSetupBytes+1))
	if err != nil {
		return nil, usageErr("--setup: cannot read %s: %v", path, err)
	}
	if len(b) > maxSetupBytes {
		return nil, usageErr("--setup %s: the script is too large (the limit is %d bytes)", path, maxSetupBytes)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, usageErr("--setup %s: the script is empty", path)
	}
	return b, nil
}

// runSetup runs the script on the new machine. Anything but a clean exit stops
// the machine again: the person was never told its id, so nothing else could,
// and it would bill until its idle timeout.
func (s *session) runSetup(ctx context.Context, id string, req transport.SetupRequest) error {
	s.em.Emit(events.Event{Name: "setup", Fields: events.Fields{"id": id, "bytes": len(req.Script)}})
	// In human mode stdout holds the machine id and nothing else, so the
	// script's output joins the progress on stderr.
	out := transport.Streams{Stdout: s.stderr, Stderr: s.stderr}
	if s.json {
		out = transport.Streams{Stdout: s.stdout, Stderr: s.stderr}
	}
	code, err := s.env.Transport.Setup(ctx, id, req, out)
	if err == nil {
		s.em.Emit(events.Event{Name: "setup_exit", Fields: events.Fields{"id": id, "code": code}})
		if code == 0 {
			return nil
		}
	}
	note := s.stopAfterFailedSetup(ctx, id)
	if err != nil {
		return fmt.Errorf("the setup script could not be run; %s: %w", note, err)
	}
	return platform.Errorf(platform.KindNotReady, "the setup script exited %d; %s", code, note)
}

// stopAfterFailedSetup stops the machine, even if the command was interrupted,
// and says what happened to it.
func (s *session) stopAfterFailedSetup(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	res, err := s.env.Transport.Stop(ctx, id)
	if err != nil {
		return fmt.Sprintf("stopping the machine failed, so %s may still be running and billing: run `runners scratch stop --id %s`", id, id)
	}
	s.em.Emit(events.Event{Name: "stopped", Fields: events.Fields{"id": id}})
	s.em.Emit(events.Event{Name: "billed", Fields: events.Fields{"seconds": res.BilledSeconds}})
	return "the machine was stopped"
}

// ---- run ----

func cmdRun(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("run")
	var (
		id     = fs.String("id", "", "machine id (or "+idEnvVar+")")
		dir    = fs.String("dir", ".", "working directory to sync")
		noSync = fs.Bool("no-sync", false, "run without syncing first")
		detach = fs.Bool("detach", false, "start the command and return without waiting for it")
		allow  stringList
		envs   stringList
		_      = fs.Bool("json", false, "events as NDJSON on stdout")
	)
	fs.Var(&allow, "allow", "send this secret-shaped file anyway (repeatable, relative to --dir)")
	fs.Var(&envs, "env", "forward this environment variable by name (repeatable)")
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	argv := fs.Args()
	if len(argv) == 0 {
		return 0, usageErr("run needs a command: runners scratch run --id ID -- <command> [args]")
	}
	machine, err := s.resolveID(*id)
	if err != nil {
		return 0, err
	}
	// Everything that can be refused locally is refused before a request.
	fwd, err := resolveEnv(envs, s.env.LookupEnv)
	if err != nil {
		return 0, err
	}
	allowPaths, err := cleanAllow(allow)
	if err != nil {
		return 0, err
	}

	if !*noSync {
		if err := s.sync(ctx, machine, *dir, allowPaths); err != nil {
			return 0, err
		}
	}

	if *detach {
		return s.startDetached(ctx, machine, transport.ExecRequest{Argv: argv, Env: fwd})
	}

	code, err := s.env.Transport.Exec(ctx, machine, transport.ExecRequest{Argv: argv, Env: fwd},
		transport.Streams{Stdout: s.stdout, Stderr: s.stderr})
	if err != nil {
		return 0, err
	}
	s.em.Emit(events.Event{Name: "exit", Fields: events.Fields{"code": code, "remote": true}})
	if platform.IsReserved(code) {
		s.em.Emit(events.Event{Name: "warning", Fields: events.Fields{
			"message": fmt.Sprintf("the command exited %d, which is also a Scratch machine status code", code),
		}})
	}
	return code, nil
}

// startDetached hands the command to the machine's supervisor and returns. The
// command's own exit code is not known yet, so there is no exit event and the
// status is the tool's: 0 means "started".
func (s *session) startDetached(ctx context.Context, id string, req transport.ExecRequest) (int, error) {
	d, err := s.env.Transport.Start(ctx, id, req)
	if err != nil {
		return 0, err
	}
	s.em.Emit(events.Event{Name: "detached", Fields: events.Fields{"id": id, "command": d.CommandID, "log": d.Log}})
	s.em.Emit(events.Event{Name: "warning", Fields: events.Fields{
		"message": "a detached command does not hold the machine open: the idle timeout still applies, and the command ends when the machine does; use `runners scratch extend` to keep it longer",
	}})
	if !s.json {
		// The command id alone on stdout, so a script can capture it.
		fmt.Fprintln(s.env.Stdout, d.CommandID)
	}
	return 0, nil
}

// cleanAllow validates the --allow list: paths inside the working directory.
func cleanAllow(allow []string) ([]string, error) {
	var out []string
	for _, a := range allow {
		p := path.Clean(filepath.ToSlash(a))
		if path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			return nil, usageErr("--allow %q: give a path inside the working directory, relative to --dir", a)
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *session) sync(ctx context.Context, id, dir string, allow []string) error {
	cands, err := manifest.List(ctx, dir)
	if err != nil {
		return fmt.Errorf("reading the working directory %s: %w", dir, err)
	}
	src := os.DirFS(dir)
	m, err := manifest.Build(src, cands, manifest.Options{
		MaxWorkspaceBytes: s.env.MaxWorkspaceBytes,
		Allow:             allow,
	})
	if err != nil {
		return err
	}
	for _, sk := range m.Skipped {
		// A vanished or non-regular file is housekeeping; the ones a person
		// might want to know about are named.
		s.em.Emit(events.Event{Name: "skipped", Fields: events.Fields{"path": sk.Path, "reason": sk.Reason}})
	}
	for _, a := range m.UnusedAllow {
		s.em.Emit(events.Event{Name: "warning", Fields: events.Fields{
			"message": fmt.Sprintf("--allow %s matched no secret-shaped file in the workspace", a),
		}})
	}
	s.em.Emit(events.Event{Name: "syncing", Fields: events.Fields{
		"files": len(m.Entries), "bytes": m.TotalBytes, "skipped": len(m.Skipped), "digest": m.Digest(),
	}})
	_, err = s.env.Transport.Sync(ctx, id, m, src)
	return err
}

// ---- ssh ----

func cmdSSH(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("ssh")
	id := fs.String("id", "", "machine id (or "+idEnvVar+")")
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	machine, err := s.resolveID(*id)
	if err != nil {
		return 0, err
	}
	return s.env.Transport.Shell(ctx, machine, fs.Args(), transport.StdIO{
		In: s.env.Stdin, Out: s.env.Stdout, Err: s.env.Stderr,
	})
}

// ---- stop ----

func cmdStop(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("stop")
	id := fs.String("id", "", "machine id (or "+idEnvVar+")")
	_ = fs.Bool("json", false, "events as NDJSON on stdout")
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, usageErr("stop takes no arguments, got %q", fs.Arg(0))
	}
	machine, err := s.resolveID(*id)
	if err != nil {
		return 0, err
	}
	res, err := s.env.Transport.Stop(ctx, machine)
	if err != nil {
		return 0, err
	}
	s.em.Emit(events.Event{Name: "stopped", Fields: events.Fields{"id": machine}})
	s.em.Emit(events.Event{Name: "billed", Fields: events.Fields{"seconds": res.BilledSeconds}})
	return 0, nil
}

// ---- status ----

func cmdStatus(ctx context.Context, s *session, args []string) (int, error) {
	fs := newFlagSet("status")
	id := fs.String("id", "", "machine id (or "+idEnvVar+")")
	_ = fs.Bool("json", false, "events as NDJSON on stdout")
	if err := parse(fs, args); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, usageErr("status takes no arguments, got %q", fs.Arg(0))
	}
	machine, err := s.resolveID(*id)
	if err != nil {
		return 0, err
	}
	st, err := s.env.Transport.Status(ctx, machine)
	if err != nil {
		return 0, err
	}
	s.em.Emit(events.Event{Name: "status", Fields: events.Fields{
		"id": st.ID, "state": st.State, "size": st.Size, "image": st.Image,
		"ready_at":      st.ReadyAt.UTC().Format(time.RFC3339),
		"idle_deadline": st.IdleDeadline.UTC().Format(time.RFC3339),
		"hard_deadline": st.HardDeadline.UTC().Format(time.RFC3339),
	}})
	return 0, nil
}

func containsInt(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func containsString(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}
