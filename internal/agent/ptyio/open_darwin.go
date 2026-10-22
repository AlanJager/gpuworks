//go:build darwin

package ptyio

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Open allocates a pty pair without cgo: open /dev/ptmx, grant and unlock the
// slave (TIOCPTYGRANT, TIOCPTYUNLK), read its name (TIOCPTYGNAME), then open
// it. The master is nonblocking and the slave blocking, as on Linux.
func Open() (master, slave *os.File, err error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	for _, req := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := unix.IoctlSetInt(mfd, req, 0); err != nil {
			_ = unix.Close(mfd)
			return nil, nil, fmt.Errorf("prepare pty slave: %w", err)
		}
	}
	name, err := slaveName(mfd)
	if err != nil {
		_ = unix.Close(mfd)
		return nil, nil, err
	}
	master = os.NewFile(uintptr(mfd), "/dev/ptmx")
	slave, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("open %s: %w", name, err)
	}
	return master, slave, nil
}

// slaveName reads the slave's path. TIOCPTYGNAME fills a 128-byte buffer,
// which no exported x/sys helper passes, so the ioctl is issued directly.
func slaveName(fd int) (string, error) {
	var buf [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", fmt.Errorf("read pty slave name: %w", errno)
	}
	if i := bytes.IndexByte(buf[:], 0); i >= 0 {
		return string(buf[:i]), nil
	}
	return string(buf[:]), nil
}
