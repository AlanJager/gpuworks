package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

// relayHub wires a store, an online node whose agent accepts image.write
// streams, and an archive site, so a relayed transfer can be observed end to
// end without a real agent process.
type relayHub struct {
	store   *store.Store
	hub     *Hub
	node    *store.Node
	image   *store.Image
	archive []byte
	url     string
	site    *ocifixture.ArchiveServer
	// loaded receives the bytes the agent read off each image.write stream.
	loaded chan []byte
	// headers receives each stream header the agent was handed.
	headers chan proto.StreamHeader
	// tempDir is the process temp directory for the duration of the test; the
	// relay must leave it untouched.
	tempDir string
}

func newRelayHub(t *testing.T, dsn, archivePath string, opts ...ocifixture.ArchiveServerOption) *relayHub {
	t.Helper()
	// The relay must not stage the archive anywhere. Point the process temp
	// directory at a directory of this test's own so "nothing was written" is
	// an assertion and not a hope.
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	st, err := store.Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("relay-node", store.HashToken("relay-token"), "relay-node")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{"registry.example/team/runner:v1"}, LayerBytes: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	site, srv := ocifixture.StartArchiveServer(archive, opts...)
	t.Cleanup(srv.Close)
	archiveURL := srv.URL + archivePath
	sources, err := json.Marshal([]string{"quay.io/team/runner:v1", srv.URL + "/image.tar"})
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{ImageRef: "registry.example/team/runner:v1", Engine: "vllm", Enabled: true, RegistrySources: string(sources)})
	if err != nil {
		t.Fatal(err)
	}

	h := New(st, slog.Default())
	h.SetOutboundAllowPrivateHosts([]string{"127.0.0.1"})
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { serverConn.Close(); clientConn.Close() })
	serverYS, clientYS := yamuxPairData(t)
	session := newSession(h, serverYS, serverConn)
	session.NodeUUID = node.UUID
	h.register(session)
	t.Cleanup(func() { h.deregister(session) })

	harness := &relayHub{
		store: st, hub: h, node: node, image: image, archive: archive,
		url: archiveURL, site: site, tempDir: tempDir,
		loaded: make(chan []byte, 4), headers: make(chan proto.StreamHeader, 4),
	}
	var acceptWG sync.WaitGroup
	acceptWG.Add(1)
	go func() {
		defer acceptWG.Done()
		for {
			stream, err := clientYS.AcceptStream()
			if err != nil {
				return
			}
			go harness.consume(stream)
		}
	}()
	// Close the client side of the yamux session to unblock AcceptStream, then
	// wait for the accept loop to actually return instead of leaking it past
	// this test. Registered after the loop starts so it runs (LIFO) before the
	// other cleanups above, which close the same session again harmlessly.
	t.Cleanup(func() {
		_ = clientYS.Close()
		acceptWG.Wait()
	})
	return harness
}

// consume plays the agent's half of an image.write stream: read the header
// line, then read the archive until the server closes its write side.
func (h *relayHub) consume(stream net.Conn) {
	defer stream.Close()
	reader := bufio.NewReader(stream)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	var header proto.StreamHeader
	if json.Unmarshal([]byte(line), &header) != nil {
		return
	}
	select {
	case h.headers <- header:
	default:
	}
	body, _ := io.ReadAll(reader)
	select {
	case h.loaded <- body:
	default:
	}
}

// assertNothingStaged fails when the relay wrote anything to disk. A
// fourteen-gigabyte archive that briefly lands in a temp file would make the
// management server's disk a hidden requirement of every import.
func (h *relayHub) assertNothingStaged(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(h.tempDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, filepath.Join(h.tempDir, entry.Name()))
	}
	if len(names) != 0 {
		t.Fatalf("the relay staged files on the server: %v", names)
	}
}

func TestCopyImageArchiveURLStreamsTheArchiveWithoutStagingIt(t *testing.T) {
	h := newRelayHub(t, "file:hub_relay_stream_test?mode=memory&cache=shared", "/image.tar")

	var progress []int64
	bytesCopied, err := h.hub.CopyImageArchiveURL(context.Background(), h.url, h.node.UUID, h.image.ImageRef, "transfer-1", func(done int64) {
		progress = append(progress, done)
	})
	if err != nil {
		t.Fatalf("CopyImageArchiveURL: %v", err)
	}
	if bytesCopied != int64(len(h.archive)) {
		t.Fatalf("copied %d bytes, want %d", bytesCopied, len(h.archive))
	}
	select {
	case header := <-h.headers:
		if header.Kind != proto.StreamImageWrite || header.Dst != h.image.ImageRef || header.Ref != "transfer-1" {
			t.Fatalf("stream header = %+v", header)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the node was never handed a load stream")
	}
	select {
	case body := <-h.loaded:
		if !bytes.Equal(body, h.archive) {
			t.Fatalf("the node received %d bytes, want the %d-byte archive", len(body), len(h.archive))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the node never received the archive")
	}
	if len(progress) == 0 || progress[len(progress)-1] != int64(len(h.archive)) {
		t.Fatalf("progress = %v, want it to end at the archive size", progress)
	}
	h.assertNothingStaged(t)
}

func TestCopyImageArchiveURLRefusesANonSuccessResponseBeforeOpeningAStream(t *testing.T) {
	h := newRelayHub(t, "file:hub_relay_status_test?mode=memory&cache=shared", "/missing.tar")

	bytesCopied, err := h.hub.CopyImageArchiveURL(context.Background(), h.url, h.node.UUID, h.image.ImageRef, "transfer-404", nil)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v, want the mirror's status", err)
	}
	if bytesCopied != 0 || !strings.Contains(err.Error(), "0 bytes transferred") {
		t.Fatalf("copied = %d err = %v", bytesCopied, err)
	}
	select {
	case header := <-h.headers:
		t.Fatalf("a failed fetch still opened a load stream: %+v", header)
	case <-time.After(300 * time.Millisecond):
	}
	h.assertNothingStaged(t)
}

func TestCopyImageArchiveURLRejectsAnUnusableAddress(t *testing.T) {
	h := newRelayHub(t, "file:hub_relay_address_test?mode=memory&cache=shared", "/image.tar")

	if _, err := h.hub.CopyImageArchiveURL(context.Background(), "ftp://mirror.example/img.tar", h.node.UUID, h.image.ImageRef, "transfer-ftp", nil); err == nil ||
		!strings.Contains(err.Error(), "not http or https") {
		t.Fatalf("error = %v", err)
	}
	if _, err := h.hub.CopyImageArchiveURL(context.Background(), h.url, "no-such-node", h.image.ImageRef, "transfer-offline", nil); err == nil ||
		!strings.Contains(err.Error(), "connected") {
		t.Fatalf("offline error = %v", err)
	}
}

func TestPullImageRelaysTheArchiveAndRecordsTheRelaySource(t *testing.T) {
	h := newRelayHub(t, "file:hub_relay_pull_test?mode=memory&cache=shared", "/image.tar")

	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{
		Source: imagecatalog.FetchSourceURL, ArchiveURL: h.url, Relay: true,
	})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if row.State != store.ImageStateSyncing || row.Source != imagecatalog.FetchSourceURLRelay {
		t.Fatalf("ledger row = %+v, want a syncing url-relay row", row)
	}
	if row.TransferID == "" || row.SizeBytes != int64(len(h.archive)) {
		t.Fatalf("ledger row = %+v, want the archive size and a transfer id", row)
	}
	select {
	case body := <-h.loaded:
		if !bytes.Equal(body, h.archive) {
			t.Fatalf("the node received %d bytes, want %d", len(body), len(h.archive))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node never received the relayed archive")
	}
	// The relay only moves bytes: the row stays syncing until the agent's own
	// load confirms, exactly as a peer transfer does.
	stored, err := h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Source != imagecatalog.FetchSourceURLRelay {
		t.Fatalf("stored row = %+v", stored)
	}
	h.assertNothingStaged(t)
}

func TestPullImageRelayRecordsTheBytesTransferredWhenTheMirrorBreaks(t *testing.T) {
	h := newRelayHub(t, "file:hub_relay_break_test?mode=memory&cache=shared", "/image.tar", ocifixture.WithMidStreamBreak(64<<10))

	if _, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{
		Source: imagecatalog.FetchSourceURL, ArchiveURL: h.url, Relay: true,
	}); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var stored *store.ImageNode
	for time.Now().Before(deadline) {
		row, err := h.store.ImageNode(h.image.ID, h.node.ID)
		if err == nil && row.State == store.ImageStateFailed {
			stored = row
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stored == nil {
		t.Fatal("a broken relay never failed the ledger row")
	}
	if !strings.Contains(stored.Message, "bytes transferred") {
		t.Fatalf("message = %q, want the bytes that made it across", stored.Message)
	}
	if !strings.Contains(stored.Message, imagecatalog.ArchiveURLHost(h.url)) {
		t.Fatalf("message = %q, want the mirror host", stored.Message)
	}
	h.assertNothingStaged(t)
}
