package runtime

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestUnavailableErrorMatchesTheSentinelAndKeepsTheCause(t *testing.T) {
	cause := fmt.Errorf("prepare udocker ownership: %w", syscall.EPERM)
	var err error = &UnavailableError{Runtime: "udocker", Err: cause}
	if !errors.Is(err, ErrRuntimeUnavailable) || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("errors.Is lost a match: %v", err)
	}
	if got, want := err.Error(), "udocker runtime unavailable: prepare udocker ownership: operation not permitted"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	// Wrapping further, as the pull path does, keeps the match.
	if !errors.Is(fmt.Errorf("pull x: %w", err), ErrRuntimeUnavailable) {
		t.Fatal("a wrapped unavailable error is not recognised")
	}
	if errors.Is(cause, ErrRuntimeUnavailable) {
		t.Fatal("a plain error matches the sentinel")
	}
}
