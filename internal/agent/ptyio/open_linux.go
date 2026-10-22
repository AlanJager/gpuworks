//go:build linux

package ptyio

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Open allocates a pty pair the way glibc's openpty(3) does, but without
// cgo: open /dev/ptmx, unlock the slave side (TIOCSPTLCK), read its number
// (TIOCGPTN), then open /dev/pts/<n>. All three ioctls have identical request
// values on amd64 and arm64, the only platforms the agent cross-compiles for.
// The master is opened O_NONBLOCK so os.NewFile registers it with the runtime
// poller. That is what makes a pending Read return when another goroutine
// calls Close: with a blocking fd a reader would stay parked in Read forever.
//
// The slave stays blocking: it becomes the child's stdio, and a nonblocking
// tty breaks programs that do not expect EAGAIN. The caller closes its copy of
// the slave once the child has started, or the master never sees the end.
func Open() (master, slave *os.File, err error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// Without unlocking first, opening the slave fails with EIO.
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(mfd)
		return nil, nil, fmt.Errorf("unlock pty slave: %w", err)
	}
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(mfd)
		return nil, nil, fmt.Errorf("read pty number: %w", err)
	}
	master = os.NewFile(uintptr(mfd), "/dev/ptmx")
	name := fmt.Sprintf("/dev/pts/%d", n)
	slave, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("open %s: %w", name, err)
	}
	return master, slave, nil
}
