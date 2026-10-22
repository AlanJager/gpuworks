//go:build linux || darwin

package ptyio

import (
	"os"

	"golang.org/x/sys/unix"
)

// Supported reports whether this build can allocate a pty.
func Supported() bool { return true }

// SetSize applies a window size to the terminal behind master.
//
// os.File.Fd() must not be used here: it reverts the descriptor to blocking
// mode and unregisters it from the poller, which would silently undo the
// interruptible-Read property Open establishes. SyscallConn keeps it.
func SetSize(master *os.File, rows, cols uint16) error {
	return control(master, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
}

// Size reads the window size back.
func Size(master *os.File) (rows, cols uint16, err error) {
	err = control(master, func(fd int) error {
		ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		if err != nil {
			return err
		}
		rows, cols = ws.Row, ws.Col
		return nil
	})
	return rows, cols, err
}

func control(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	if err := rc.Control(func(fd uintptr) { inner = fn(int(fd)) }); err != nil {
		return err
	}
	return inner
}
