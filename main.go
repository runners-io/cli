// Command runners is the command line of the Scratch machine (`runners scratch`): a
// CI-class microVM you sync your working directory to, run commands in, and
// stop. The design lives in the engine repository (services/ghwarp/docs/design/,
// the file named for the internal codename, which this tree deliberately does
// not repeat).
//
// STATUS: A SKELETON. NOT PUBLISHED, NOT SOLD, NOT STAGED ON ANY NODE. The
// service it would talk to does not exist, so the only transport wired in is
// transport.Unavailable, which refuses every request with exit code 125. The
// command table, the exit codes, the event stream and the sync manifest are
// real and tested against an in-memory fake (internal/transport/fake).
//
// The working binary name is a placeholder: it needs a collision check before
// it is announced (design section 0).
//
// This directory is written to be lifted into an open-source repository
// as it is: it imports only the standard library and itself, and
// main_test.go fails the build if that stops being true.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/runners-io/cli/internal/cli"
	"github.com/runners-io/cli/internal/credentials"
	"github.com/runners-io/cli/internal/transport"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.Env{
		Args:        os.Args[1:],
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		LookupEnv:   os.LookupEnv,
		Now:         time.Now,
		Transport:   transport.Unavailable{},
		Credentials: credentials.File{Path: credentials.DefaultPath(os.LookupEnv)},
	})
	stop()
	os.Exit(code)
}
