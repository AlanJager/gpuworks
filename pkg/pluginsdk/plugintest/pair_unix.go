//go:build unix

package plugintest

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// socketPair returns two connected stream sockets, the transport a plugin
// gets from the server.
func socketPair() (net.Conn, net.Conn, error) {
	fds, err := cloexecSocketpair()
	if err != nil {
		return nil, nil, err
	}
	a, err := fileConn(fds[0], "plugintest-a")
	if err != nil {
		_ = syscall.Close(fds[1])
		return nil, nil, err
	}
	b, err := fileConn(fds[1], "plugintest-b")
	if err != nil {
		_ = a.Close()
		return nil, nil, err
	}
	return a, b, nil
}

// cloexecSocketpair creates a socketpair whose ends are close-on-exec
// before any other goroutine can fork, so no child inherits an end and
// keeps the connection open after the other side hangs up.
func cloexecSocketpair() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fds, fmt.Errorf("plugintest: socketpair: %w", err)
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	return fds, nil
}

func fileConn(fd int, name string) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return net.FileConn(f)
}

// filePair returns two connected stream sockets: the server's end as a
// connection, and the end a plugin process inherits as a file.
func filePair() (net.Conn, *os.File, error) {
	fds, err := cloexecSocketpair()
	if err != nil {
		return nil, nil, err
	}
	core, err := fileConn(fds[0], "plugintest-core")
	if err != nil {
		_ = syscall.Close(fds[1])
		return nil, nil, err
	}
	return core, os.NewFile(uintptr(fds[1]), "plugintest-plugin"), nil
}
