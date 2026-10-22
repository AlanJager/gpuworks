// Package ptyio allocates pseudo-terminal pairs without cgo. The web terminal
// runs a shell on one; the docker driver runs `docker pull` on one, because
// the CLI prints per-layer byte counters only to a terminal.
package ptyio

import (
	"errors"
	"syscall"
)

// ErrUnsupported is returned on platforms with no pty support in this build.
// The agent ships for Linux and macOS; the stubs keep the package compiling
// for any other host a developer builds on.
var ErrUnsupported = errors.New("pseudo-terminals are only supported on linux and macOS nodes")

// IsClosed reports whether err is the EIO that Linux and macOS return when
// reading a master whose last slave fd has closed. That is the normal end of
// the child's output, not a failure.
func IsClosed(err error) bool {
	return errors.Is(err, syscall.EIO)
}
