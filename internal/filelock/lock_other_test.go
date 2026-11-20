//go:build !linux && !darwin

package filelock

import (
	"errors"
	"testing"
)

func TestUnsupportedPlatformRejectsAcquisition(t *testing.T) {
	release, err := Acquire("unused.lock")
	if release != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported platform accepted a lock: %v", err)
	}
}
