package distribution

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// objectStore is an httptest stand-in for an object store's public endpoint:
// GET serves an object with http.ServeContent (200, or 206 with Content-Range
// for a range, 416 past the end, as OSS, OBS and R2 do), a missing key is a
// 404 with an S3 error document, and per-object faults reproduce what a bad
// link or a misbehaving mirror does.
type objectStore struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string][]byte
	faults  map[string]fault
	// requests records "<path> <Range header>" for every GET.
	requests []string
	// queries records the query string of every GET, in the same order.
	queries []string
	server  *httptest.Server
}

type fault struct {
	// delay holds the response headers back.
	delay time.Duration
	// cutAfter closes the connection after this many body bytes.
	cutAfter int
	// stallAfter stops sending after this many body bytes until the client
	// gives up.
	stallAfter int
	// ignoreRange answers a range request with the whole object.
	ignoreRange bool
	// status answers with this status and an S3 error body.
	status int
	// once makes the fault apply to the first request only.
	once bool
}

func newObjectStore(t *testing.T) *objectStore {
	t.Helper()
	s := &objectStore{t: t, objects: map[string][]byte{}, faults: map[string]fault{}}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *objectStore) URL() string { return s.server.URL }

func (s *objectStore) put(path string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[path] = data
}

func (s *objectStore) fail(path string, f fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[path] = f
}

// queryLog is the query string of every request, in order.
func (s *objectStore) queryLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

func (s *objectStore) requestLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *objectStore) serve(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	s.mu.Lock()
	s.requests = append(s.requests, key+" "+r.Header.Get("Range"))
	s.queries = append(s.queries, r.URL.RawQuery)
	data, ok := s.objects[key]
	f, faulty := s.faults[key]
	if faulty && f.once {
		delete(s.faults, key)
	}
	s.mu.Unlock()
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}
	if f.status != 0 {
		writeS3Error(w, f.status, "AccessDenied")
		return
	}
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	switch {
	case f.cutAfter > 0 || f.stallAfter > 0:
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		limit := f.cutAfter
		if f.stallAfter > 0 {
			limit = f.stallAfter
		}
		_, _ = w.Write(data[:limit])
		w.(http.Flusher).Flush()
		if f.stallAfter > 0 {
			<-r.Context().Done()
			return
		}
		// Drop the connection mid-body, as a reset link does.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	case f.ignoreRange:
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	default:
		http.ServeContent(w, r, key, time.Unix(0, 0), bytes.NewReader(data))
	}
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>fixture</Message></Error>`))
}

// testKey is a signing key generated per test binary.
var testKeyID, testPublic, testPrivate = func() (string, ed25519.PublicKey, ed25519.PrivateKey) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return "test-key", public, private
}()

func testKeys() Keys { return Keys{testKeyID: testPublic} }

// blob is file content plus the index entry describing it.
func blob(path string, data []byte) File {
	sum := sha256.Sum256(data)
	return File{Path: path, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

func testIndex(channel string, files ...ReleaseFile) Index {
	return Index{
		Schema: IndexSchema, Channel: channel, PublishedAt: time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC),
		Server: ServerRelease{ReleaseID: "abc1234", Commit: "abc1234def", BuiltAt: "2026-10-11T01:00:00Z", Files: files},
	}
}

// publish signs index with the test key and stores it on every store.
func publish(t *testing.T, index Index, stores ...*objectStore) {
	t.Helper()
	payload, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Seal(payload, testKeyID, testPrivate)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stores {
		s.put(index.Channel+"/index.json", envelope)
	}
}

func clientFor(stores ...*objectStore) *Client {
	sources := make([]string, 0, len(stores))
	for _, s := range stores {
		sources = append(sources, s.URL())
	}
	return &Client{Sources: sources, Keys: testKeys(), IndexTimeout: 2 * time.Second, StallTimeout: 300 * time.Millisecond}
}
