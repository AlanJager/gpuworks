package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// noStorageStreams answers the storage-files stream methods for fakes that
// do not take part in them: no lane, every stream refused.
type noStorageStreams struct{}

func (noStorageStreams) DataLaneAttached(string) bool { return false }

func (noStorageStreams) UploadStorageFile(context.Context, string, proto.StreamHeader, io.Reader) (proto.StorageFilesStatus, error) {
	return proto.StorageFilesStatus{}, errors.New("storage upload not part of this test")
}

func (noStorageStreams) OpenStorageDownload(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
	return proto.StorageDownloadHead{}, nil, errors.New("storage download not part of this test")
}

// fakeStorageStreams is the hub as the storage-files handlers see it. Upload
// and download default to a faithful agent: an upload reads the whole body
// and reports what it received; a download refuses with not_found.
type fakeStorageStreams struct {
	mu       sync.Mutex
	attached map[string]bool
	upload   func(ctx context.Context, uuid string, hdr proto.StreamHeader, body io.Reader) (proto.StorageFilesStatus, error)
	download func(ctx context.Context, uuid string, hdr proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error)
	headers  []proto.StreamHeader
	nodes    []string
}

func (f *fakeStorageStreams) OpenLogtail(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("logtail not used")
}

func (f *fakeStorageStreams) OpenLargeFileRead(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("large file read not used")
}

func (f *fakeStorageStreams) UpgradeAgent(string, string, string) (bool, error) {
	return false, errors.New("upgrade not used")
}

func (f *fakeStorageStreams) DataLaneAttached(uuid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attached[uuid]
}

func (f *fakeStorageStreams) setAttached(uuid string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached[uuid] = on
}

func (f *fakeStorageStreams) record(uuid string, hdr proto.StreamHeader) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, hdr)
	f.nodes = append(f.nodes, uuid)
}

func (f *fakeStorageStreams) lastHeader(t *testing.T) (string, proto.StreamHeader) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.headers) == 0 {
		t.Fatal("no stream was opened")
	}
	return f.nodes[len(f.nodes)-1], f.headers[len(f.headers)-1]
}

func (f *fakeStorageStreams) opened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.headers)
}

func (f *fakeStorageStreams) UploadStorageFile(ctx context.Context, uuid string, hdr proto.StreamHeader, body io.Reader) (proto.StorageFilesStatus, error) {
	f.record(uuid, hdr)
	if f.upload != nil {
		return f.upload(ctx, uuid, hdr, body)
	}
	n, err := io.Copy(io.Discard, body)
	if err != nil {
		return proto.StorageFilesStatus{}, err
	}
	if hdr.Size >= 0 && n != hdr.Size {
		return proto.StorageFilesStatus{State: proto.StorageFilesFailed, Code: proto.StorageFilesErrFailed, Error: "size mismatch", Bytes: n}, nil
	}
	return proto.StorageFilesStatus{State: proto.StorageFilesDone, Bytes: n, Files: 1}, nil
}

func (f *fakeStorageStreams) OpenStorageDownload(ctx context.Context, uuid string, hdr proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
	f.record(uuid, hdr)
	if f.download != nil {
		return f.download(ctx, uuid, hdr)
	}
	return proto.StorageDownloadHead{Code: proto.StorageFilesErrNotFound, Error: "no such file"}, nil, nil
}

type storageFilesCall struct {
	node   string
	method string
	params json.RawMessage
}

// storageFilesRig is a router over a real store with two nodes, a shared and
// a local storage ready (and writable) on both, and fakes for the hub.
type storageFilesRig struct {
	t       *testing.T
	st      *store.Store
	h       http.Handler
	admin   string
	member  string
	streams *fakeStorageStreams
	shared  *store.Storage
	local   *store.Storage
	n1, n2  *store.Node

	mu     sync.Mutex
	online map[string]bool
	caps   map[string][]string
	calls  []storageFilesCall
	answer func(method string, params json.RawMessage) (json.RawMessage, error)
}

func newStorageFilesRig(t *testing.T, logs ...*slog.Logger) *storageFilesRig {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, err := auth.NewTokenSigner("storage-files-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: "member", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	rig := &storageFilesRig{t: t, st: st, admin: admin, member: member,
		streams: &fakeStorageStreams{attached: map[string]bool{}}, online: map[string]bool{}, caps: map[string][]string{}}
	rig.n1, err = st.RegisterNode("gpu1", store.HashToken("a"), "gpu1")
	if err != nil {
		t.Fatal(err)
	}
	rig.n2, err = st.RegisterNode("gpu2", store.HashToken("b"), "gpu2")
	if err != nil {
		t.Fatal(err)
	}
	rig.shared, err = st.CreateStorage(&store.Storage{Name: "team", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/team", Export: "nfs.example.internal:/export/team"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.local, err = st.CreateStorage(&store.Storage{Name: "scratch", Type: proto.StorageLocal, Enabled: true, AllNodes: true, Path: "/data/scratch"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*store.Node{rig.n1, rig.n2} {
		rig.online[n.UUID] = true
		rig.caps[n.UUID] = []string{proto.CapStorageFiles}
		rig.streams.attached[n.UUID] = true
		rig.setStorageState(n, true, true)
	}
	var log *slog.Logger
	if len(logs) > 0 {
		log = logs[0]
	}
	rig.h = Router(Deps{Store: st, AuthSigner: signer, NodeStreams: rig.streams, Log: log,
		Online: func(uuid string) bool {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			return rig.online[uuid]
		},
		NodeCaps: func(uuid string) []string {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			return rig.caps[uuid]
		},
		CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
			raw, _ := json.Marshal(params)
			rig.mu.Lock()
			rig.calls = append(rig.calls, storageFilesCall{node: uuid, method: method, params: raw})
			answer := rig.answer
			rig.mu.Unlock()
			if answer != nil {
				return answer(method, raw)
			}
			if method == proto.RPCStorageFilesList {
				return json.RawMessage(`{"entries":[]}`), nil
			}
			return json.RawMessage(`{}`), nil
		},
	})
	return rig
}

// setStorageState reports both storages on n as ready or not, writable or not.
func (r *storageFilesRig) setStorageState(n *store.Node, ready, writable bool) {
	r.t.Helper()
	state := proto.StorageStateReady
	if !ready {
		state = proto.StorageStateFailed
	}
	if err := r.st.ApplyStorageStatus(n.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: r.shared.ID, State: state, Path: r.shared.Path, Writable: writable},
		{ID: r.local.ID, State: state, Path: r.local.Path, Writable: writable},
	}}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *storageFilesRig) setOnline(uuid string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.online[uuid] = on
}

func (r *storageFilesRig) setCaps(uuid string, caps ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.caps[uuid] = caps
}

func (r *storageFilesRig) setAnswer(fn func(method string, params json.RawMessage) (json.RawMessage, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answer = fn
}

func (r *storageFilesRig) lastCall() storageFilesCall {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		r.t.Fatal("no RPC was sent")
	}
	return r.calls[len(r.calls)-1]
}

func (r *storageFilesRig) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *storageFilesRig) disable() {
	r.t.Helper()
	disabled := false
	if _, err := r.st.UpdateStorage(r.shared.ID, store.StoragePatch{Enabled: &disabled}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *storageFilesRig) do(token, method, target string, body io.Reader, header ...string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, target, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *storageFilesRig) url(storage *store.Storage, suffix string) string {
	return fmt.Sprintf("/api/v1/storages/%d/files%s", storage.ID, suffix)
}

// errorCode reads the product error code of a JSON error response.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not a JSON error (%d): %q", rec.Code, rec.Body.String())
	}
	return body.Code
}

func expectError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d body = %s, want %d %s", rec.Code, rec.Body, status, code)
	}
	if got := errorCode(t, rec); got != code {
		t.Fatalf("code = %q body = %s, want %q", got, rec.Body, code)
	}
}

// touchedBody records whether a handler read the request body at all.
type touchedBody struct {
	io.Reader
	mu      sync.Mutex
	touched bool
}

func (b *touchedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.touched = true
	b.mu.Unlock()
	return b.Reader.Read(p)
}

func (b *touchedBody) wasRead() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.touched
}
