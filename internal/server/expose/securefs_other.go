//go:build !linux && !darwin

package expose

import "os"

func ensureStateDirectory(string) error      { return errUnsupportedPlatform }
func openStateLock(string) (*os.File, error) { return nil, errUnsupportedPlatform }

func writeStateFileAtomic(string, []byte, os.FileMode) error { return errUnsupportedPlatform }
