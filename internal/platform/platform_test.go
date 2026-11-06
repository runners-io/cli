package platform

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The exit codes are a contract with scripts and coding agents, so they are
// pinned by value here and not only by name: a renumbering must be a failing
// test, never a quiet change.
func TestExitCodesArePinned(t *testing.T) {
	want := map[Kind]int{
		KindUsage:        120,
		KindSyncTooLarge: 121,
		KindQuota:        122,
		KindAuth:         123,
		KindSessionLost:  124,
		KindNotReady:     125,
	}
	for k, code := range want {
		if got := k.ExitCode(); got != code {
			t.Errorf("%s exits %d, want %d", k, got, code)
		}
	}
	if len(want) != len(AllKinds()) {
		t.Fatalf("AllKinds has %d kinds, the pin table has %d: a kind was added without a pinned code", len(AllKinds()), len(want))
	}
}

func TestReservedRangeIsExactlyTheKindsCodes(t *testing.T) {
	seen := map[int]Kind{}
	for _, k := range AllKinds() {
		c := k.ExitCode()
		if !IsReserved(c) {
			t.Errorf("%s exits %d, outside the reserved range %d-%d", k, c, ReservedLow, ReservedHigh)
		}
		if prev, dup := seen[c]; dup {
			t.Errorf("%s and %s share exit code %d", prev, k, c)
		}
		seen[c] = k
	}
	for _, c := range []int{0, 1, 2, 119, 126, 127, 130, 255} {
		if IsReserved(c) {
			t.Errorf("%d is reported reserved; a remote command may exit with it", c)
		}
	}
}

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, 0},
		{"platform error", Errorf(KindAuth, "no token"), 123},
		{"wrapped platform error", fmt.Errorf("while syncing: %w", Errorf(KindSyncTooLarge, "big")), 121},
		{"cancelled is 130", context.Canceled, 130},
		{"wrapped cancel is 130", fmt.Errorf("x: %w", context.Canceled), 130},
		{"anything else is a usage-range failure", errors.New("boom"), 120},
	}
	for _, c := range cases {
		if got := ExitCodeFor(c.err); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestErrorKindSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("outer: %w", Errorf(KindQuota, "3 boxes already"))
	k, ok := KindOf(err)
	if !ok || k != KindQuota {
		t.Fatalf("KindOf = %v, %v", k, ok)
	}
	if _, ok := KindOf(errors.New("plain")); ok {
		t.Fatal("a plain error has no kind")
	}
}
