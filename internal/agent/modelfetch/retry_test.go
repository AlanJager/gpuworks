package modelfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestTransientErrorClassification(t *testing.T) {
	timeout := &net.OpError{Op: "read", Net: "tcp", Err: timeoutError{}}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	for _, err := range []error{
		io.EOF,
		fmt.Errorf("read response: %w", io.ErrUnexpectedEOF),
		context.DeadlineExceeded,
		timeout,
		reset,
		errors.New("net/http: TLS handshake timeout"),
	} {
		if !IsTransient(err) {
			t.Errorf("transient error was rejected: %T %v", err, err)
		}
	}
	for _, err := range []error{
		context.Canceled,
		errors.New("download: unexpected probe status 401"),
		errors.New("checksum mismatch"),
		os.ErrPermission,
	} {
		if IsTransient(err) {
			t.Errorf("permanent/caller error was classified transient: %T %v", err, err)
		}
	}
}

func TestLocalFailuresAreTheNodes(t *testing.T) {
	for _, err := range []error{
		&os.PathError{Op: "open", Path: "/models/x.part", Err: syscall.EACCES},
		fmt.Errorf("write: %w", syscall.ENOSPC),
		&os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EXDEV},
	} {
		if !isLocal(err) {
			t.Errorf("%v is the node's own disk", err)
		}
	}
	if isLocal(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}) {
		t.Error("a refused connection is the address's")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "fixture timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
