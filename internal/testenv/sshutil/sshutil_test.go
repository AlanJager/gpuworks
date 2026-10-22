package sshutil

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type keepaliveRecorder struct {
	calls atomic.Int32
	errAt int32
}

func (r *keepaliveRecorder) SendRequest(name string, wantReply bool, payload []byte) (bool, []byte, error) {
	if name != "keepalive@openssh.com" || !wantReply || payload != nil {
		return false, nil, fmt.Errorf("unexpected keepalive request name=%q want_reply=%t payload=%v", name, wantReply, payload)
	}
	call := r.calls.Add(1)
	if r.errAt > 0 && call >= r.errAt {
		return false, nil, errors.New("transport closed")
	}
	return false, nil, nil
}

func TestSendSSHKeepalivesContinuesAfterUnsupportedReply(t *testing.T) {
	recorder := &keepaliveRecorder{}
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() { result <- sendSSHKeepalives(done, recorder, time.Millisecond) }()
	deadline := time.Now().Add(time.Second)
	for recorder.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(done)
	if err := <-result; err != nil {
		t.Fatalf("keepalive stopped after unsupported reply: %v", err)
	}
	if calls := recorder.calls.Load(); calls < 2 {
		t.Fatalf("keepalive calls=%d, want at least 2", calls)
	}
}

func TestSendSSHKeepalivesReturnsTransportError(t *testing.T) {
	recorder := &keepaliveRecorder{errAt: 2}
	done := make(chan struct{})
	if err := sendSSHKeepalives(done, recorder, time.Millisecond); err == nil || err.Error() != "transport closed" {
		t.Fatalf("keepalive error=%v, want transport closed", err)
	}
}

func TestSendSSHKeepalivesRejectsInvalidInterval(t *testing.T) {
	if err := sendSSHKeepalives(make(chan struct{}), &keepaliveRecorder{}, 0); err == nil {
		t.Fatal("zero keepalive interval unexpectedly accepted")
	}
}

func TestCaptureSessionOutputSeparatesConcurrentStreams(t *testing.T) {
	session := new(ssh.Session)
	output := captureSessionOutput(session)
	if session.Stdout == session.Stderr {
		t.Fatal("stdout and stderr must not share a writer")
	}

	const writes = 1000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range writes {
			_, _ = session.Stdout.Write([]byte("stdout\n"))
		}
	}()
	go func() {
		defer wg.Done()
		for range writes {
			_, _ = session.Stderr.Write([]byte("stderr\n"))
		}
	}()
	wg.Wait()

	got := output.String()
	if strings.ContainsRune(got, '\x00') {
		t.Fatalf("captured output contains NUL bytes: %q", got)
	}
	if strings.Count(got, "stdout\n") != writes || strings.Count(got, "stderr\n") != writes {
		t.Fatalf("captured line counts = stdout:%d stderr:%d, want %d each", strings.Count(got, "stdout\n"), strings.Count(got, "stderr\n"), writes)
	}
}

func TestProxyPairWaitsForBothDirections(t *testing.T) {
	left, leftPeer := net.Pipe()
	right, rightPeer := net.Pipe()
	defer leftPeer.Close()
	defer rightPeer.Close()
	done := make(chan struct{})
	go func() {
		proxyPair(left, right)
		close(done)
	}()

	go func() {
		_, _ = leftPeer.Write([]byte("request"))
		buf := make([]byte, len("response"))
		if _, err := leftPeer.Read(buf); err != nil || string(buf) != "response" {
			t.Errorf("left response = %q, %v", buf, err)
		}
		_ = leftPeer.Close()
	}()
	go func() {
		buf := make([]byte, len("request"))
		if _, err := rightPeer.Read(buf); err != nil || string(buf) != "request" {
			t.Errorf("right request = %q, %v", buf, err)
		}
		if _, err := rightPeer.Write([]byte("response")); err != nil {
			t.Errorf("right response: %v", err)
		}
		_ = rightPeer.Close()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal(fmt.Errorf("proxyPair did not finish after both directions closed"))
	}
}

func TestForwardTargetAcceptsURLAndTCPAddress(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "http://127.0.0.1:8199", want: "127.0.0.1:8199"},
		{input: "https://[::1]:8443/path", want: "[::1]:8443"},
		{input: "127.0.0.1:9000", want: "127.0.0.1:9000"},
	} {
		got, err := forwardTarget(test.input)
		if err != nil || got != test.want {
			t.Fatalf("forwardTarget(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	if _, err := forwardTarget("http://127.0.0.1"); err == nil {
		t.Fatal("forward target without a port should fail")
	}
}
