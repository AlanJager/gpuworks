package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// newStorageFilesHub is a hub with one registered node whose data lane is
// attached; the returned yamux session is the agent's end of that lane.
func newStorageFilesHub(t *testing.T) (*Hub, string, *yamux.Session) {
	t.Helper()
	h, uuid := newLargeFileHub(t)
	data, agent := yamuxPairData(t)
	h.session(uuid).attachData(data)
	waitState(t, h.session(uuid), "attached")
	return h, uuid, agent
}

// fakeStorageAgent accepts one stream on the agent's lane and hands it, with
// its header already read, to serve.
func fakeStorageAgent(t *testing.T, agent *yamux.Session, serve func(hdr proto.StreamHeader, r *bufio.Reader, s *yamux.Stream)) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		stream, err := agent.AcceptStream()
		if err != nil {
			t.Errorf("agent accept: %v", err)
			return
		}
		r := bufio.NewReader(stream)
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Errorf("agent read header: %v", err)
			return
		}
		var hdr proto.StreamHeader
		if err := json.Unmarshal(line, &hdr); err != nil {
			t.Errorf("agent header: %v", err)
			return
		}
		serve(hdr, r, stream)
	}()
	return done
}

// resetWitnessAgent accepts one stream and reads it to the end, header
// included, reporting how the read ended. A reset can reach the agent before
// it has read the header, so the header is not parsed apart: whatever the
// agent got, it must not be a clean end.
func resetWitnessAgent(t *testing.T, agent *yamux.Session) <-chan error {
	t.Helper()
	ended := make(chan error, 1)
	go func() {
		stream, err := agent.AcceptStream()
		if err != nil {
			ended <- err
			return
		}
		_, err = io.ReadAll(stream)
		ended <- err
	}()
	return ended
}

func waitEnded(t *testing.T, ended <-chan error) error {
	t.Helper()
	select {
	case err := <-ended:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("fake agent did not finish")
		return nil
	}
}

func writeLine(t *testing.T, w io.Writer, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Error(err)
		return
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		t.Errorf("agent write line: %v", err)
	}
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake agent did not finish")
	}
}

// The body crosses as is, the header names the target, and the agent's answer
// is read only after the half-close it waits for. Repeated, because the
// answer and the end of the copy race and either may be seen first.
func TestUploadStorageFileSendsBodyHalfClosesAndReadsTheVerdict(t *testing.T) {
	h, uuid, agent := newStorageFilesHub(t)
	for i := 0; i < 20; i++ {
		body := bytes.Repeat([]byte("abc"), 100_000)
		done := fakeStorageAgent(t, agent, func(hdr proto.StreamHeader, r *bufio.Reader, s *yamux.Stream) {
			defer s.Close()
			if hdr.Kind != proto.StreamStorageUpload || hdr.StorageID != 7 || hdr.Rel != "files/a.bin" || hdr.Staging != ".gpuworks/incoming/cp/upload-1" || hdr.Size != int64(len(body)) || hdr.Extract != "" || !hdr.Overwrite {
				t.Errorf("header = %+v", hdr)
			}
			got, err := io.ReadAll(r) // ends only at the server's half-close
			if err != nil || !bytes.Equal(got, body) {
				t.Errorf("agent read %d bytes err=%v", len(got), err)
			}
			writeLine(t, s, proto.StorageFilesStatus{State: proto.StorageFilesDone, Bytes: int64(len(got)), Files: 1})
		})
		status, err := h.UploadStorageFile(context.Background(), uuid, proto.StreamHeader{StorageID: 7, Rel: "files/a.bin", Staging: ".gpuworks/incoming/cp/upload-1", Size: int64(len(body)), Overwrite: true}, bytes.NewReader(body))
		if err != nil || status.State != proto.StorageFilesDone || status.Bytes != int64(len(body)) || status.Files != 1 {
			t.Fatalf("round %d: status = %+v err = %v", i, status, err)
		}
		waitDone(t, done)
	}
}

// endless is a body that never ends on its own.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// An agent that refuses before the body is through (no space, a bad archive)
// answers and stops reading; the server returns its refusal rather than
// blocking on the window or reporting the broken write.
func TestUploadStorageFileReturnsAnEarlyRefusal(t *testing.T) {
	h, uuid, agent := newStorageFilesHub(t)
	done := fakeStorageAgent(t, agent, func(_ proto.StreamHeader, r *bufio.Reader, s *yamux.Stream) {
		_, _ = io.CopyN(io.Discard, r, 1<<20)
		writeLine(t, s, proto.StorageFilesStatus{State: proto.StorageFilesFailed, Code: proto.StorageFilesErrNotWritable, Error: "no space"})
		// A refusal is followed by a close, never a reset: yamux drops
		// unread bytes on a reset, the answer line with them.
		_ = s.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := h.UploadStorageFile(ctx, uuid, proto.StreamHeader{StorageID: 1, Rel: "files/big", Size: -1}, endless{})
	if err != nil || status.State != proto.StorageFilesFailed || status.Code != proto.StorageFilesErrNotWritable || status.Error != "no space" {
		t.Fatalf("status = %+v err = %v", status, err)
	}
	waitDone(t, done)
}

type failingBody struct {
	sent bool
}

func (b *failingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial"), nil
	}
	return 0, errors.New("client went away")
}

// A body that breaks is not an end of file: the stream is reset, so the agent
// never commits a truncated upload of unknown size.
func TestUploadStorageFileResetsTheStreamWhenTheBodyBreaks(t *testing.T) {
	h, uuid, agent := newStorageFilesHub(t)
	ended := resetWitnessAgent(t, agent)
	_, err := h.UploadStorageFile(context.Background(), uuid, proto.StreamHeader{StorageID: 1, Rel: "files/x", Size: -1}, &failingBody{})
	if err == nil || !strings.Contains(err.Error(), "client went away") {
		t.Fatalf("err = %v, want the body's error", err)
	}
	if got := waitEnded(t, ended); got == nil {
		t.Fatal("the agent read a clean end of a broken upload")
	}
}

// The request ending (client disconnected) mid-body resets the stream too.
func TestUploadStorageFileCancelResetsTheStream(t *testing.T) {
	h, uuid, agent := newStorageFilesHub(t)
	ended := resetWitnessAgent(t, agent)
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { _, _ = pw.Write([]byte("first chunk")) }()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := h.UploadStorageFile(ctx, uuid, proto.StreamHeader{StorageID: 1, Rel: "files/x", Size: -1}, pr)
		result <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	_ = pw.CloseWithError(context.Canceled) // what net/http does to a dead client's body
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a canceled upload reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled upload did not return")
	}
	if got := waitEnded(t, ended); got == nil {
		t.Fatal("the agent read a clean end of a canceled upload")
	}
}

// The agent's answer must be a status line; anything else is a transport
// failure, and a stream that ends without one is too.
func TestUploadStorageFileRejectsABadOrMissingAnswer(t *testing.T) {
	for name, answer := range map[string]string{
		"not json":      "nonsense\n",
		"unknown state": `{"state":"maybe"}` + "\n",
		"no answer":     "",
	} {
		t.Run(name, func(t *testing.T) {
			h, uuid, agent := newStorageFilesHub(t)
			done := fakeStorageAgent(t, agent, func(_ proto.StreamHeader, r *bufio.Reader, s *yamux.Stream) {
				_, _ = io.ReadAll(r)
				_, _ = io.WriteString(s, answer)
				_ = s.Close()
			})
			if status, err := h.UploadStorageFile(context.Background(), uuid, proto.StreamHeader{StorageID: 1, Rel: "files/x", Size: 2}, strings.NewReader("hi")); err == nil {
				t.Fatalf("status = %+v, want an error", status)
			}
			waitDone(t, done)
		})
	}
}

// Without an attached data lane nothing opens: no fallback to ctrl, no wait.
func TestStorageStreamsNeedAnAttachedDataLane(t *testing.T) {
	h, uuid := newLargeFileHub(t)
	if h.DataLaneAttached(uuid) || h.DataLaneAttached("unknown") {
		t.Fatal("no lane is attached")
	}
	if _, err := h.UploadStorageFile(context.Background(), uuid, proto.StreamHeader{}, strings.NewReader("x")); !errors.Is(err, ErrDataLaneUnavailable) {
		t.Fatalf("upload err = %v", err)
	}
	if _, _, err := h.OpenStorageDownload(context.Background(), uuid, proto.StreamHeader{}); !errors.Is(err, ErrDataLaneUnavailable) {
		t.Fatalf("download err = %v", err)
	}
	if _, err := h.UploadStorageFile(context.Background(), "unknown", proto.StreamHeader{}, strings.NewReader("x")); err == nil {
		t.Fatal("an unknown node opened a stream")
	}
	data, _ := yamuxPairData(t)
	h.session(uuid).attachData(data)
	waitState(t, h.session(uuid), "attached")
	if !h.DataLaneAttached(uuid) {
		t.Fatal("attached lane not reported")
	}
}

func TestOpenStorageDownloadReadsTheHeadThenTheBytes(t *testing.T) {
	h, uuid, agent := newStorageFilesHub(t)
	done := fakeStorageAgent(t, agent, func(hdr proto.StreamHeader, _ *bufio.Reader, s *yamux.Stream) {
		if hdr.Kind != proto.StreamStorageDownload || hdr.StorageID != 3 || hdr.Rel != "files/d" || hdr.Format != proto.StorageArchiveTarGz {
			t.Errorf("header = %+v", hdr)
		}
		writeLine(t, s, proto.StorageDownloadHead{Dir: true, Size: -1, Name: "d"})
		_, _ = io.WriteString(s, "archive bytes")
		_ = s.Close()
	})
	head, body, err := h.OpenStorageDownload(context.Background(), uuid, proto.StreamHeader{StorageID: 3, Rel: "files/d", Format: proto.StorageArchiveTarGz})
	if err != nil || !head.Dir || head.Size != -1 || head.Name != "d" || body == nil {
		t.Fatalf("head = %+v body = %v err = %v", head, body, err)
	}
	got, err := io.ReadAll(body)
	if err != nil || string(got) != "archive bytes" {
		t.Fatalf("body = %q err = %v", got, err)
	}
	_ = body.Close()
	waitDone(t, done)
}

func TestOpenStorageDownloadReturnsARefusalWithoutABody(t *testing.T) {
	h, uuid, agent := newStorageFilesHub(t)
	done := fakeStorageAgent(t, agent, func(_ proto.StreamHeader, _ *bufio.Reader, s *yamux.Stream) {
		writeLine(t, s, proto.StorageDownloadHead{Code: proto.StorageFilesErrNotFound, Error: "no such file"})
		_ = s.Close()
	})
	head, body, err := h.OpenStorageDownload(context.Background(), uuid, proto.StreamHeader{StorageID: 3, Rel: "files/none"})
	if err != nil || body != nil || head.Code != proto.StorageFilesErrNotFound || head.Error != "no such file" {
		t.Fatalf("head = %+v body = %v err = %v", head, body, err)
	}
	waitDone(t, done)
}

// Closing a download before its end resets the stream: the agent's next
// write fails instead of filling a window nobody drains.
func TestStorageDownloadCloseBeforeTheEndResetsTheStream(t *testing.T) {
	h, uuid, agent := newStorageFilesHub(t)
	writeErr := make(chan error, 1)
	done := fakeStorageAgent(t, agent, func(_ proto.StreamHeader, _ *bufio.Reader, s *yamux.Stream) {
		writeLine(t, s, proto.StorageDownloadHead{Size: 1 << 40, Name: "huge"})
		chunk := bytes.Repeat([]byte("y"), 32<<10)
		for {
			if _, err := s.Write(chunk); err != nil {
				writeErr <- err
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, body, err := h.OpenStorageDownload(ctx, uuid, proto.StreamHeader{StorageID: 1, Rel: "files/huge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, body, 1<<20); err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("agent write kept succeeding")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent's writes were never stopped")
	}
	waitDone(t, done)
}

// An agent that never writes the head does not hold the request forever.
func TestStorageDownloadHeadWaitIsBounded(t *testing.T) {
	server, agentSide := net.Pipe()
	defer agentSide.Close()
	start := time.Now()
	_, _, err := readDownloadHead(context.Background(), server, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("head wait not bounded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	server2, agent2 := net.Pipe()
	defer agent2.Close()
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, _, err := readDownloadHead(ctx, server2, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err = %v", err)
	}
	server3, agent3 := net.Pipe()
	go func() { _, _ = io.WriteString(agent3, "garbage\n"); _ = agent3.Close() }()
	if _, _, err := readDownloadHead(context.Background(), server3, time.Minute); err == nil {
		t.Fatal("a garbage head was accepted")
	}
}
