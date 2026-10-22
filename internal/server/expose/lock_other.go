//go:build !linux && !darwin

package expose

import (
	"errors"
	"os"
)

// The exposure feature needs a connector binary that pkg.cloudflare.com only
// publishes for Linux and macOS; on any other platform the package still
// builds so the rest of the server does.
var errUnsupportedPlatform = errors.New("the public entry point requires Linux or macOS")

func tryLockFile(*os.File) (bool, error) { return false, errUnsupportedPlatform }

func unlockFile(*os.File) error { return errUnsupportedPlatform }
