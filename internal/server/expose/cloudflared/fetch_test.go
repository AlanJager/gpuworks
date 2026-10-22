package cloudflared

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// payload returns incompressible bytes so a truncation is a real one.
func payload(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return buf
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// recordingServer serves data with Range support and remembers what it saw.
type recordingServer struct {
	mu       sync.Mutex
	requests []string // the Range header of each request
	handler  func(w http.ResponseWriter, r *http.Request, attempt int)
}

func (s *recordingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	attempt := len(s.requests) + 1
	s.requests = append(s.requests, r.Header.Get("Range"))
	s.mu.Unlock()
	s.handler(w, r, attempt)
}

func (s *recordingServer) ranges() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func TestDownloadVerifiesAndRenames(t *testing.T) {
	data := payload(t, 256<<10)
	rec := &recordingServer{handler: func(w http.ResponseWriter, r *http.Request, _ int) {
		http.ServeContent(w, r, "pkg", time.Now(), bytes.NewReader(data))
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.deb")
	var progress []int64
	err := Download(context.Background(), srv.Client(), DownloadRequest{
		URL:      srv.URL + "/pkg.deb",
		Dest:     dest,
		SHA256:   digestOf(data),
		Size:     int64(len(data)),
		Progress: func(done, _ int64) { progress = append(progress, done) },
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(data))
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("the partial file must be renamed away, not left behind")
	}
	if len(progress) == 0 {
		t.Error("progress was never reported")
	}
}

// TestDownloadResumesAfterInterruption is the case the measured link actually
// produces: the transfer stops partway, and the next attempt continues with a
// Range rather than starting over.
func TestDownloadResumesAfterInterruption(t *testing.T) {
	data := payload(t, 256<<10)
	rec := &recordingServer{handler: func(w http.ResponseWriter, r *http.Request, attempt int) {
		if attempt == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data[:len(data)/3])
			return // the connection closes early
		}
		http.ServeContent(w, r, "pkg", time.Now(), bytes.NewReader(data))
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.deb")
	err := Download(context.Background(), srv.Client(), DownloadRequest{
		URL:        srv.URL + "/pkg.deb",
		Dest:       dest,
		SHA256:     digestOf(data),
		Size:       int64(len(data)),
		RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) || digestOf(got) != digestOf(data) {
		t.Fatalf("resumed download is wrong: %d bytes", len(got))
	}
	ranges := rec.ranges()
	if len(ranges) < 2 {
		t.Fatalf("expected a second attempt, saw %d requests", len(ranges))
	}
	if ranges[1] == "" {
		t.Error("the retry must carry a Range header to resume instead of restarting")
	}
}

func TestDownloadRejectsWrongDigest(t *testing.T) {
	data := payload(t, 64<<10)
	rec := &recordingServer{handler: func(w http.ResponseWriter, r *http.Request, _ int) {
		http.ServeContent(w, r, "pkg", time.Now(), bytes.NewReader(data))
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.deb")
	err := Download(context.Background(), srv.Client(), DownloadRequest{
		URL:        srv.URL + "/pkg.deb",
		Dest:       dest,
		SHA256:     digestOf([]byte("something else")),
		Size:       int64(len(data)),
		Attempts:   1,
		RetryDelay: time.Millisecond,
	})
	if err == nil {
		t.Fatal("a digest mismatch must fail the download")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("nothing may be renamed into place when the digest does not match")
	}
	if _, statErr := os.Stat(dest + ".part"); statErr == nil {
		t.Error("bytes known to be wrong must not be kept for a resume")
	}
}

func TestDownloadRejectsWrongSize(t *testing.T) {
	data := payload(t, 32<<10)
	rec := &recordingServer{handler: func(w http.ResponseWriter, r *http.Request, _ int) {
		http.ServeContent(w, r, "pkg", time.Now(), bytes.NewReader(data))
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	err := Download(context.Background(), srv.Client(), DownloadRequest{
		URL:        srv.URL + "/pkg.deb",
		Dest:       filepath.Join(t.TempDir(), "pkg.deb"),
		Size:       int64(len(data)) + 1,
		Attempts:   1,
		RetryDelay: time.Millisecond,
	})
	if err == nil {
		t.Fatal("a size mismatch must fail the download")
	}
}

func TestDownloadRetriesServerErrors(t *testing.T) {
	data := payload(t, 32<<10)
	rec := &recordingServer{handler: func(w http.ResponseWriter, r *http.Request, attempt int) {
		if attempt < 3 {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, "pkg", time.Now(), bytes.NewReader(data))
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.deb")
	err := Download(context.Background(), srv.Client(), DownloadRequest{
		URL:        srv.URL + "/pkg.deb",
		Dest:       dest,
		SHA256:     digestOf(data),
		RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Download should have retried past the 500s: %v", err)
	}
	if len(rec.ranges()) != 3 {
		t.Errorf("requests = %d, want 3 (two failures then success)", len(rec.ranges()))
	}
}

func TestDownloadHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := Download(ctx, srv.Client(), DownloadRequest{
		URL:        srv.URL + "/pkg.deb",
		Dest:       filepath.Join(t.TempDir(), "pkg.deb"),
		Attempts:   5,
		RetryDelay: time.Second,
	})
	if err == nil {
		t.Fatal("a cancelled download must stop retrying")
	}
}
