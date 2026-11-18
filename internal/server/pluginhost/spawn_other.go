//go:build !linux && !darwin

package pluginhost

import (
	"errors"
	"os"
	"syscall"
)

var errUnsupported = errors.New("plugins run only on Linux and macOS servers")

func socketPair() (*os.File, *os.File, error) { return nil, nil, errUnsupported }

func signalGroup(int, os.Signal) error { return errUnsupported }

func sysProcAttr() *syscall.SysProcAttr { return nil }
