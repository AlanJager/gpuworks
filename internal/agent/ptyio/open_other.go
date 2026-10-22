//go:build !linux && !darwin

package ptyio

import "os"

// Supported reports whether this build can allocate a pty.
func Supported() bool { return false }

// Open is unsupported on this platform.
func Open() (master, slave *os.File, err error) { return nil, nil, ErrUnsupported }

// SetSize is unsupported on this platform.
func SetSize(master *os.File, rows, cols uint16) error { return ErrUnsupported }

// Size is unsupported on this platform.
func Size(master *os.File) (rows, cols uint16, err error) { return 0, 0, ErrUnsupported }
