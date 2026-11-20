package cluster

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var existingOpenHandshake = []byte{1, 0, 0, 0, 0, 0, 0, 0}

func existingOpenLiteral() []byte {
	return []byte{
		5, 0, 0, 0, 3, 0, 0, 0,
		'g', 'p', 'u', 'w', 'o', 'r', 'k', 's', '.', 'd', 'b', 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		'v', 'o', 'l', 'a', 't', 'i', 'l', 'e', 0, 0, 0, 0, 0, 0, 0, 0,
	}
}

func TestExistingReadSQLLiteralSplitAndCoalescedWrites(t *testing.T) {
	for _, chunk := range []int{1, 3, 8, 48, 1024} {
		t.Run(fmt.Sprintf("chunk-%d", chunk), func(t *testing.T) {
			client, peer := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
			_ = client.SetDeadline(time.Now().Add(3 * time.Second))
			conn := wrapExistingReadSQLConnection(client)
			// Literal SDK LEADER and CLIENT requests precede OPEN unchanged.
			control := []byte{
				1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				1, 0, 0, 0, 1, 0, 0, 0, 7, 0, 0, 0, 0, 0, 0, 0,
			}
			input := append(append(append([]byte{}, existingOpenHandshake...), control...), existingOpenLiteral()...)
			want := bytes.Clone(input)
			want[8+len(control)+4] = 20
			peerResult := make(chan error, 1)
			go func() {
				got := make([]byte, len(want))
				_, err := io.ReadFull(peer, got)
				if err == nil && !bytes.Equal(got, want) {
					err = errors.New("wire changed control or sent ordinary OPEN")
				}
				if err == nil {
					_, err = peer.Write([]byte{1, 0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
				}
				peerResult <- err
			}()
			for len(input) > 0 {
				next := min(chunk, len(input))
				if n, err := conn.Write(input[:next]); err != nil || n != next {
					t.Fatalf("Write = %d, %v", n, err)
				}
				input = input[next:]
			}
			got := make([]byte, 16)
			for i := range got {
				if _, err := io.ReadFull(conn, got[i:i+1]); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(got, []byte{1, 0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
				t.Fatalf("response changed: %x", got)
			}
			if err := <-peerResult; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExistingReadSQLOldServerRejectsWithoutNormalOpen(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	conn := wrapExistingReadSQLConnection(client)
	// Failure code 1 and literal padded native error text.
	failure := []byte{3, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0,
		'u', 'n', 'k', 'n', 'o', 'w', 'n', ' ', 't', 'y', 'p', 'e', 0, 0, 0, 0}
	peerResult := make(chan error, 1)
	go func() {
		request := make([]byte, 56)
		_, err := io.ReadFull(peer, request)
		if err == nil && (request[12] != 20 || !bytes.Equal(request[:8], existingOpenHandshake)) {
			err = errors.New("old native was offered ordinary OPEN")
		}
		if err == nil {
			_, err = peer.Write(failure)
		}
		peerResult <- err
	}()
	if _, err := conn.Write(append(bytes.Clone(existingOpenHandshake), existingOpenLiteral()...)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(failure))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, failure) {
		t.Fatalf("native rejection was changed: %x %v", got, err)
	}
	if err := <-peerResult; err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(existingOpenLiteral()); err == nil {
		t.Fatal("rejected OPEN retried using normal OPEN")
	}
}

type existingOpenRecordingConn struct {
	bytes.Buffer
	read       *bytes.Reader
	writeLimit int
	writeErr   error
	closeCalls atomic.Int32
	closeErr   error
	deadline   time.Time
}

func (c *existingOpenRecordingConn) Read(p []byte) (int, error) { return c.read.Read(p) }
func (c *existingOpenRecordingConn) Write(p []byte) (int, error) {
	if c.writeLimit > 0 {
		p = p[:min(len(p), c.writeLimit)]
	}
	n, _ := c.Buffer.Write(p)
	return n, c.writeErr
}
func (c *existingOpenRecordingConn) Close() error                       { c.closeCalls.Add(1); return c.closeErr }
func (c *existingOpenRecordingConn) LocalAddr() net.Addr                { return nil }
func (c *existingOpenRecordingConn) RemoteAddr() net.Addr               { return nil }
func (c *existingOpenRecordingConn) SetDeadline(d time.Time) error      { c.deadline = d; return nil }
func (c *existingOpenRecordingConn) SetReadDeadline(d time.Time) error  { return c.SetDeadline(d) }
func (c *existingOpenRecordingConn) SetWriteDeadline(d time.Time) error { return c.SetDeadline(d) }

func existingOpenRecorder(t *testing.T) (*existingOpenRecordingConn, net.Conn) {
	t.Helper()
	raw := &existingOpenRecordingConn{read: bytes.NewReader(nil)}
	conn := wrapExistingReadSQLConnection(raw)
	if _, err := conn.Write(existingOpenHandshake); err != nil {
		t.Fatal(err)
	}
	return raw, conn
}

func TestExistingReadSQLRejectsMalformedOpenBeforeSendingHeader(t *testing.T) {
	tests := map[string]func([]byte) []byte{
		"schema":       func(b []byte) []byte { b[5] = 1; return b },
		"extra":        func(b []byte) []byte { b[6] = 1; return b },
		"flags":        func(b []byte) []byte { b[24] = 1; return b },
		"vfs":          func(b []byte) []byte { b[32] = 'x'; return b },
		"padding":      func(b []byte) []byte { b[23] = 1; return b },
		"empty-name":   func(b []byte) []byte { b[8] = 0; return b },
		"slash":        func(b []byte) []byte { b[9] = '/'; return b },
		"backslash":    func(b []byte) []byte { b[9] = '\\'; return b },
		"URI-colon":    func(b []byte) []byte { b[9] = ':'; return b },
		"query":        func(b []byte) []byte { b[9] = '?'; return b },
		"control":      func(b []byte) []byte { b[9] = 1; return b },
		"invalid-UTF8": func(b []byte) []byte { b[9] = 255; return b },
		"no-name-terminator": func(b []byte) []byte {
			for i := 8; i < 24; i++ {
				b[i] = 'x'
			}
			return b
		},
		"oversized":          func(b []byte) []byte { binary.LittleEndian.PutUint32(b, 68); return b[:8] },
		"zero-words":         func(b []byte) []byte { binary.LittleEndian.PutUint32(b, 0); return b[:8] },
		"unknown-type":       func(b []byte) []byte { b[4] = 21; return b },
		"pre-rewritten-type": func(b []byte) []byte { b[4] = 20; return b },
		"write-before-open":  func(b []byte) []byte { b[4] = 8; return b },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			raw, conn := existingOpenRecorder(t)
			if _, err := conn.Write(mutate(existingOpenLiteral())); err == nil {
				t.Fatal("unsafe OPEN accepted")
			}
			if !bytes.Equal(raw.Bytes(), existingOpenHandshake) || raw.closeCalls.Load() != 0 {
				t.Fatal("invalid OPEN sent bytes or stole Close ownership")
			}
			if _, err := conn.Write(existingOpenLiteral()); err == nil || raw.Len() != 8 {
				t.Fatal("error reset allowed fallback")
			}
		})
	}
}

func TestExistingReadSQLShortWritesAndCausesRemainSticky(t *testing.T) {
	for _, failure := range []error{nil, errors.New("native socket write failure")} {
		t.Run(fmtErrorName(failure), func(t *testing.T) {
			raw, conn := existingOpenRecorder(t)
			raw.writeLimit, raw.writeErr = 3, failure
			_, err := conn.Write(existingOpenLiteral())
			if !errors.Is(err, io.ErrShortWrite) || failure != nil && !errors.Is(err, failure) {
				t.Fatalf("short write/cause lost: %v", err)
			}
			before := raw.Len()
			if n, next := conn.Write(existingOpenLiteral()); n != 0 || !errors.Is(next, io.ErrShortWrite) || raw.Len() != before {
				t.Fatalf("short write retried: %d %v", n, next)
			}
		})
	}
}

func fmtErrorName(err error) string {
	if err == nil {
		return "short-nil"
	}
	return "short-error"
}

func TestExistingReadSQLRejectsHandshakeAndRepeatedOpen(t *testing.T) {
	t.Run("legacy-handshake", func(t *testing.T) {
		raw := &existingOpenRecordingConn{read: bytes.NewReader(nil)}
		conn := wrapExistingReadSQLConnection(raw)
		if _, err := conn.Write([]byte{229, 63, 67, 96, 215, 77, 16, 134}); err == nil || raw.Len() != 0 {
			t.Fatal("legacy handshake accepted", err)
		}
	})
	t.Run("second-open", func(t *testing.T) {
		raw, conn := existingOpenRecorder(t)
		if _, err := conn.Write(existingOpenLiteral()); err != nil {
			t.Fatal(err)
		}
		before := raw.Len()
		if _, err := conn.Write(existingOpenLiteral()); err == nil || raw.Len() != before {
			t.Fatal("second OPEN sent", err)
		}
	})
}

func TestExistingReadSQLValidatesDatabaseResponse(t *testing.T) {
	for _, field := range []string{"schema", "extra", "size", "id", "reserved", "type", "truncated"} {
		t.Run(field, func(t *testing.T) {
			raw, conn := existingOpenRecorder(t)
			if _, err := conn.Write(existingOpenLiteral()); err != nil {
				t.Fatal(err)
			}
			response := []byte{1, 0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
			switch field {
			case "schema":
				response[5] = 1
			case "extra":
				response[6] = 1
			case "size":
				response[0] = 2
			case "id":
				response[8] = 1
			case "reserved":
				response[12] = 1
			case "type":
				response[4] = 2
			case "truncated":
				response = response[:12]
			}
			raw.read = bytes.NewReader(response)
			if n, err := conn.Read(make([]byte, 16)); n != 0 || err == nil {
				t.Fatalf("invalid DB response leaked: %d %v", n, err)
			}
		})
	}
}

func TestExistingReadSQLRejectsMalformedFailureResponse(t *testing.T) {
	for _, body := range [][]byte{
		{1, 0, 0, 0, 0, 0, 0, 0},
		{1, 0, 0, 0, 0, 0, 0, 0, 'e', 'r', 'r', 'o', 'r', 'x', 'x', 'x'},
		{1, 0, 0, 0, 0, 0, 0, 0, 'e', 'r', 'r', 0, 1, 0, 0, 0},
	} {
		t.Run(fmt.Sprintf("body-%x", body), func(t *testing.T) {
			raw, conn := existingOpenRecorder(t)
			if _, err := conn.Write(existingOpenLiteral()); err != nil {
				t.Fatal(err)
			}
			header := []byte{0, 0, 0, 0, 0, 0, 0, 0}
			binary.LittleEndian.PutUint32(header, uint32(len(body)/8))
			raw.read = bytes.NewReader(append(header, body...))
			if n, err := conn.Read(make([]byte, 16)); n != 0 || err == nil {
				t.Fatalf("malformed SDK failure leaked: %d %v", n, err)
			}
		})
	}
}

func TestExistingReadSQLCloseDeadlineAndPostOpenQuery(t *testing.T) {
	raw, conn := existingOpenRecorder(t)
	deadline := time.Now().Add(time.Second)
	if err := conn.SetDeadline(deadline); err != nil || raw.deadline != deadline {
		t.Fatal("original deadline was not preserved")
	}
	if _, err := conn.Write(existingOpenLiteral()); err != nil {
		t.Fatal(err)
	}
	query := []byte{2, 0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 'S', 'E', 'L', 'E', 'C', 'T', 0, 0}
	if _, err := conn.Write(query); err != nil || !bytes.Equal(raw.Bytes()[56:], query) {
		t.Fatal("ordinary query changed", err)
	}
	failure := errors.New("original Close acknowledgement")
	raw.closeErr = failure
	if err := conn.Close(); !errors.Is(err, failure) || raw.closeCalls.Load() != 1 {
		t.Fatal("Close ownership/cause lost", err)
	}
	if _, err := conn.Write([]byte{1}); !errors.Is(err, net.ErrClosed) {
		t.Fatal("closed adapter buffered a write", err)
	}
}

func TestExistingReadSQLFilenameBounds(t *testing.T) {
	for _, name := range []string{strings.Repeat("a", 511), strings.Repeat("a", 512), ".", ".."} {
		t.Run(fmt.Sprint(len(name), "-", name[:min(2, len(name))]), func(t *testing.T) {
			body := append([]byte(name), 0)
			body = append(body, make([]byte, (8-len(body)%8)%8)...)
			body = append(body, make([]byte, 8)...)
			body = append(body, []byte{'v', 'o', 'l', 'a', 't', 'i', 'l', 'e', 0, 0, 0, 0, 0, 0, 0, 0}...)
			frame := append([]byte{0, 0, 0, 0, 3, 0, 0, 0}, body...)
			binary.LittleEndian.PutUint32(frame, uint32(len(body)/8))
			raw, conn := existingOpenRecorder(t)
			_, err := conn.Write(frame)
			if len(name) == 511 {
				if err != nil || raw.Bytes()[12] != 20 {
					t.Fatal("valid maximum filename refused", err)
				}
			} else if err == nil || raw.Len() != 8 {
				t.Fatal("unsafe filename reached wire", err)
			}
		})
	}
}

func TestExistingReadSQLCanceledWriteUsesOriginalOwner(t *testing.T) {
	owner := socketOwnerFixture(t)
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	signaled := &existingOpenBlockedWriteConn{Conn: client, entered: make(chan struct{})}
	owned, err := owner.dial(t.Context(), "local.example:9001", func(context.Context, string) (net.Conn, error) { return signaled, nil })
	if err != nil {
		t.Fatal(err)
	}
	conn := wrapExistingReadSQLConnection(owned)
	peerStarted := make(chan struct{})
	go func() {
		var handshake [8]byte
		_, _ = io.ReadFull(peer, handshake[:])
		close(peerStarted)
	}()
	if _, err := conn.Write(existingOpenHandshake); err != nil {
		t.Fatal(err)
	}
	<-peerStarted
	writeDone := make(chan error, 1)
	go func() { _, err := conn.Write(existingOpenLiteral()); writeDone <- err }()
	<-signaled.entered
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("actual owner cancellation did not end Write", err)
	}
}

type existingOpenBlockedWriteConn struct {
	net.Conn
	entered chan struct{}
}

func (c *existingOpenBlockedWriteConn) Write(p []byte) (int, error) {
	if len(p) > 8 {
		close(c.entered)
	}
	return c.Conn.Write(p)
}

func TestExistingReadSQLCloseJoinsOriginalAcknowledgement(t *testing.T) {
	owner := socketOwnerFixture(t)
	client, peer := net.Pipe()
	failure := errors.New("held original close failed")
	held := &quorumHeldCloseConn{Conn: client, entered: make(chan struct{}), release: make(chan struct{}), failure: failure}
	var once sync.Once
	release := func() { once.Do(func() { close(held.release) }) }
	t.Cleanup(func() { release(); _ = peer.Close() })
	owned, err := owner.dial(t.Context(), "local.example:9001", func(context.Context, string) (net.Conn, error) { return held, nil })
	if err != nil {
		t.Fatal(err)
	}
	conn := wrapExistingReadSQLConnection(owned)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- conn.Close() }()
	<-held.entered
	go func() { second <- conn.Close() }()
	select {
	case err := <-first:
		t.Fatal("Close skipped actual acknowledgement", err)
	default:
	}
	select {
	case err := <-second:
		t.Fatal("second Close skipped actual acknowledgement", err)
	default:
	}
	release()
	for _, result := range []<-chan error{first, second} {
		if err := <-result; !errors.Is(err, failure) {
			t.Fatal("Close cause lost", err)
		}
	}
	if held.calls.Load() != 1 {
		t.Fatal("original owner repeated Close")
	}
}
