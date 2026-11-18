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
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("plugintest: socketpair: %w", err)
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

func fileConn(fd int, name string) (net.Conn, error) {
	syscall.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return net.FileConn(f)
}
