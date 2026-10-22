//go:build !linux && !darwin

package selfinstall

import (
	"context"
	"errors"
	"os"
)

var errUnsupportedPlatform = errors.New("install is supported on Linux and macOS only")

func startDetached(context.Context, Command) (int, error) {
	return 0, errUnsupportedPlatform
}

func freeBytes(string) (uint64, error) {
	return 0, errUnsupportedPlatform
}

// lockFile is a no-op outside Linux and macOS; the same-machine upgrades this
// guards are not a scenario there.
func lockFile(*os.File) (bool, error) { return true, nil }

func unlockFile(*os.File) error { return nil }

func processAlive(int) bool { return false }

func signalProcess(int, string) error { return errUnsupportedPlatform }
