//go:build linux || darwin

package app

import (
	"path/filepath"
	"testing"
)

func TestCopyTargetLockRetainedUntilRelease(t *testing.T) {
	target := filepath.Join(t.TempDir(), "model")
	release, err := lockCopyTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	other, err := lockCopyTarget(target)
	if err == nil {
		other()
		release()
		t.Fatal("second file descriptor acquired the live writer lock")
	}
	release()
	other, err = lockCopyTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	other()
}
