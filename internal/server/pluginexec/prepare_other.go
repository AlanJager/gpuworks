//go:build !linux && !darwin

package pluginexec

import (
	"errors"
	"log/slog"
)

// LandlockABI is 0 where plugins are not supported.
func LandlockABI() int { return 0 }

// prepare refuses: plugins run only on Linux and macOS servers.
func prepare(*Profile, *slog.Logger) error {
	return errors.New("plugins run only on Linux and macOS")
}
