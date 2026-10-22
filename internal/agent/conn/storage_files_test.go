package conn

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/agent/storagefiles"
	"gpuworks.ai/gpuworks/internal/proto"
)

// storageFilesHandlers serves the storage streams with the real service over
// a temporary storage root; the rest of Handlers does nothing.
type storageFilesHandlers struct {
	inboundHandlers
	svc *storagefiles.Service
	// failAfterHead makes a download fail once its head is out.
	failAfterHead bool
}

func (h *storageFilesHandlers) ReceiveStorageUpload(ctx context.Context, hdr proto.StreamHeader, rw io.ReadWriter) error {
	return h.svc.Upload(ctx, hdr, rw)
}

func (h *storageFilesHandlers) SendStorageDownload(ctx context.Context, hdr proto.StreamHeader, w io.Writer) error {
	if h.failAfterHead {
		_, _ = io.WriteString(w, `{"dir":true,"size":-1,"name":"d"}`+"\n"+"PK partial")
		return errors.New("disk read failed")
	}
	return h.svc.Download(ctx, hdr, w)
}

type storageStreamRig struct {
	root    string
	client  *yamux.Session
	server  *yamux.Session
	conn    *Conn
	handler *storageFilesHandlers
}

func newStorageStreamRig(t *testing.T) *storageStreamRig {
	t.Helper()
	root := t.TempDir()
	clientConn, serverConn := net.Pipe()
	// The server opens streams, as in production, and the agent accepts them.
	srv, err := yamux.Client(serverConn, yamux.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := yamux.Server(clientConn, yamux.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close(); agent.Close() })
	h := &storageFilesHandlers{svc: storagefiles.New(storagefiles.Options{
		Root: func(uint) (storagefiles.Root, error) {
			return storagefiles.Root{Dir: root, Ready: true, Writable: true}, nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})}
	return &storageStreamRig{root: root, client: srv, server: agent, conn: &Conn{handlers: h, log: slog.New(slog.NewTextHandler(io.Discard, nil))}, handler: h}
}

// open opens one stream from the server side, writes its header and runs
// the agent's handler for it; done closes when the handler returned.
func (r *storageStreamRig) open(t *testing.T, hdr proto.StreamHeader) (*yamux.Stream, chan struct{}) {
	t.Helper()
	stream, err := r.client.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		accepted, err := r.server.AcceptStream()
		if err == nil {
			r.conn.handleStream(context.Background(), accepted)
		}
	}()
	line, _ := json.Marshal(hdr)
	if _, err := stream.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	return stream, done
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent's stream handler did not return")
	}
}

func TestStorageUploadOverYamuxAnswersAfterTheBody(t *testing.T) {
	r := newStorageStreamRig(t)
	body := strings.Repeat("data", 64<<10) // 256 KiB: more than one yamux window
	stream, done := r.open(t, proto.StreamHeader{Kind: proto.StreamStorageUpload, StorageID: 1, Rel: "files/a.bin",
		Staging: proto.StorageUploadStagingRel("cp1", "u1"), Size: int64(len(body))})
	if _, err := io.WriteString(stream, body); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	all, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	var st proto.StorageFilesStatus
	if err := json.Unmarshal([]byte(strings.TrimSuffix(string(all), "\n")), &st); err != nil {
		t.Fatalf("answer %q: %v", all, err)
	}
	if st.State != proto.StorageFilesDone || st.Bytes != int64(len(body)) || st.Files != 1 {
		t.Fatalf("status = %+v", st)
	}
	waitDone(t, done)
	got, err := os.ReadFile(filepath.Join(r.root, "files", "a.bin"))
	if err != nil || string(got) != body {
		t.Fatalf("stored %d bytes, %v", len(got), err)
	}
}

func TestStorageUploadRefusalIsReadableBeforeTheBodyEnds(t *testing.T) {
	r := newStorageStreamRig(t)
	if err := os.MkdirAll(filepath.Join(r.root, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.root, "files", "a"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream, done := r.open(t, proto.StreamHeader{Kind: proto.StreamStorageUpload, StorageID: 1, Rel: "files/a",
		Staging: proto.StorageUploadStagingRel("cp1", "u2"), Size: 1 << 30})
	// A server reading the answer while it sends sees the refusal at once ...
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(stream).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var st proto.StorageFilesStatus
	_ = json.Unmarshal([]byte(line), &st)
	if st.State != proto.StorageFilesFailed || st.Code != proto.StorageFilesErrExists {
		t.Fatalf("status = %+v", st)
	}
	// ... and stops sending; the agent's handler ends with the stream.
	_ = stream.Close()
	waitDone(t, done)
}

func TestStorageUploadBrokenMidBodyCleansUp(t *testing.T) {
	r := newStorageStreamRig(t)
	staging := proto.StorageUploadStagingRel("cp1", "u3")
	stream, done := r.open(t, proto.StreamHeader{Kind: proto.StreamStorageUpload, StorageID: 1, Rel: "files/a",
		Staging: staging, Size: 1 << 20})
	if _, err := io.WriteString(stream, strings.Repeat("x", 1000)); err != nil {
		t.Fatal(err)
	}
	// Wait until the agent has started staging, then the server goes away.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(r.root, staging, "body")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = stream.Reset()
	waitDone(t, done)
	if _, err := os.Lstat(filepath.Join(r.root, staging)); !os.IsNotExist(err) {
		t.Fatalf("staging left behind: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(r.root, "files", "a")); !os.IsNotExist(err) {
		t.Fatalf("a broken upload created its target: %v", err)
	}
}

func TestStorageDownloadOverYamux(t *testing.T) {
	r := newStorageStreamRig(t)
	if err := os.MkdirAll(filepath.Join(r.root, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.root, "files", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) (string, error) {
		stream, done := r.open(t, proto.StreamHeader{Kind: proto.StreamStorageDownload, StorageID: 1, Rel: rel})
		_ = stream.CloseWrite()
		_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
		all, err := io.ReadAll(stream)
		waitDone(t, done)
		return string(all), err
	}
	got, err := read("files/a.txt")
	if err != nil || got != `{"dir":false,"size":5,"name":"a.txt"}`+"\nhello" {
		t.Fatalf("file download = %q, %v", got, err)
	}
	// A refusal: the head arrives and the stream ends cleanly.
	got, err = read("files/missing")
	if err != nil || !strings.Contains(got, `"code":"not_found"`) || !strings.HasSuffix(got, "}\n") {
		t.Fatalf("refusal = %q, %v", got, err)
	}
	// A failure after the head resets the stream: the server's read fails
	// rather than ending as if the archive were complete.
	r.handler.failAfterHead = true
	_, err = read("files")
	if !errors.Is(err, yamux.ErrStreamReset) {
		t.Fatalf("truncated download read = %v, want a reset", err)
	}
}
