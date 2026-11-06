// Package platform names the ways the Scratch machine service, or the CLI's own
// checks, can refuse a request, and the process exit code each one maps to.
//
// A remote command's own exit code is passed through as the CLI's exit code.
// The codes here sit in a reserved range so a script can tell "the platform
// refused" from "your command failed" in the common case. The range is
// ASSUMED (the design marks 120-125 as a starting point, not a commitment),
// which is why every code is pinned by value in the tests.
package platform

import (
	"context"
	"errors"
	"fmt"
)

// Kind is one class of platform-side refusal.
type Kind int

const (
	// KindUsage is a bad command line or an input the CLI refused before any
	// request was made. It is also the code for an unclassified internal error.
	KindUsage Kind = iota + 1
	// KindSyncTooLarge: the workspace is over the sync limit.
	KindSyncTooLarge
	// KindQuota: the organisation's box cap, the fleet cap or billing refused.
	KindQuota
	// KindAuth: no credential, or one the service refused.
	KindAuth
	// KindSessionLost: the box is gone, expired or was stopped.
	KindSessionLost
	// KindNotReady: the box (or the service) is not ready to take the request.
	KindNotReady
)

// Reserved exit-code range for the platform. A remote command can still exit
// with a code in it; the `exit` event under --json carries the unambiguous
// value, see the README.
const (
	ReservedLow  = 120
	ReservedHigh = 125
)

// ExitCodeInterrupted is the conventional 128+SIGINT.
const ExitCodeInterrupted = 130

// AllKinds lists every kind, in code order.
func AllKinds() []Kind {
	return []Kind{KindUsage, KindSyncTooLarge, KindQuota, KindAuth, KindSessionLost, KindNotReady}
}

// ExitCode is the process exit code for the kind.
func (k Kind) ExitCode() int {
	switch k {
	case KindUsage:
		return 120
	case KindSyncTooLarge:
		return 121
	case KindQuota:
		return 122
	case KindAuth:
		return 123
	case KindSessionLost:
		return 124
	case KindNotReady:
		return 125
	}
	return 120
}

// String is the stable machine name used in the `error` event.
func (k Kind) String() string {
	switch k {
	case KindUsage:
		return "usage"
	case KindSyncTooLarge:
		return "sync_too_large"
	case KindQuota:
		return "quota_or_billing"
	case KindAuth:
		return "auth"
	case KindSessionLost:
		return "session_lost"
	case KindNotReady:
		return "not_ready"
	}
	return "unknown"
}

// IsReserved reports whether code is in the platform's reserved range.
func IsReserved(code int) bool { return code >= ReservedLow && code <= ReservedHigh }

// Error is a platform refusal. Its message is shown to the user, so it must
// never contain a credential or an environment value.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

// Errorf builds an Error.
func Errorf(k Kind, format string, a ...any) *Error {
	return &Error{Kind: k, Message: fmt.Sprintf(format, a...)}
}

// KindOf finds the Kind of err, looking through wrapping.
func KindOf(err error) (Kind, bool) {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Kind, true
	}
	return 0, false
}

// ExitCodeFor maps an error to the process exit code. nil is 0; a platform
// error is its kind's code; a cancelled context is 130; anything else is the
// usage code, because an error the CLI did not classify is the CLI's fault and
// must not look like the remote command's.
func ExitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	if k, ok := KindOf(err); ok {
		return k.ExitCode()
	}
	if errors.Is(err, context.Canceled) {
		return ExitCodeInterrupted
	}
	return KindUsage.ExitCode()
}
