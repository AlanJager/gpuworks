//go:build linux && cgo && dqlite

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// isolatedFront is a socket this process owns in front of an isolated node.
// dqlite accepts its private abstract socket only from the process that bound
// it, so the server cannot dial the node directly. The front forwards bytes
// unchanged and admits only local connections of this process's own user,
// the same boundary that protects the data directory.
type isolatedFront struct {
	listener net.Listener
	backend  string
	done     chan struct{}
	conns    sync.WaitGroup
}

func serveIsolatedFront(backend string) (*isolatedFront, error) {
	if len(backend) < 2 || backend[0] != '@' {
		return nil, errors.New("isolated node did not bind a private Unix socket")
	}
	// The kernel refuses a second bind of the same abstract name, so the pid
	// and the clock are only there to keep names of different runs apart.
	listener, err := net.Listen("unix", fmt.Sprintf("@gpuworks-isolated-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err != nil {
		return nil, err
	}
	front := &isolatedFront{listener: listener, backend: backend, done: make(chan struct{})}
	go front.accept()
	return front, nil
}

func (f *isolatedFront) Addr() net.Addr { return f.listener.Addr() }

// Close stops accepting, cuts every forwarded connection and waits for them,
// so nothing still talks to the node when the caller closes it.
func (f *isolatedFront) Close() error {
	err := f.listener.Close()
	<-f.done
	f.conns.Wait()
	return err
}

func (f *isolatedFront) accept() {
	defer close(f.done)
	stop := make(chan struct{})
	defer close(stop)
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.conns.Add(1)
		go func() {
			defer f.conns.Done()
			f.forward(conn.(*net.UnixConn), stop)
		}()
	}
}

func (f *isolatedFront) forward(conn *net.UnixConn, stop <-chan struct{}) {
	defer conn.Close()
	if err := sameUser(conn); err != nil {
		return
	}
	dialed, err := net.Dial("unix", f.backend)
	if err != nil {
		return
	}
	node := dialed.(*net.UnixConn)
	defer node.Close()
	finished := make(chan struct{})
	go func() {
		select {
		case <-stop:
			_ = conn.Close()
			_ = node.Close()
		case <-finished:
		}
	}()
	defer close(finished)
	upstream := make(chan struct{})
	go func() {
		defer close(upstream)
		_, _ = io.Copy(node, conn)
		_ = node.CloseWrite()
	}()
	_, _ = io.Copy(conn, node)
	_ = conn.CloseWrite()
	<-upstream
}

// sameUser reads the kernel's record of who connected.
func sameUser(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peer *unix.Ucred
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if peerErr != nil {
		return peerErr
	}
	if peer.Uid != uint32(os.Getuid()) {
		return errors.New("isolated node refuses another user")
	}
	return nil
}
