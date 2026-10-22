package imagearchive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func peekFixture(t *testing.T, srv *httptest.Server) (Peek, error) {
	t.Helper()
	// Reading the complete body does not wait for the handler's final byte
	// accounting. Close joins its handlers before callers inspect their totals.
	defer srv.Close()
	return PeekURL(context.Background(), srv.Client(), srv.URL+"/image.tar")
}

func rangeReq(start, end int64) string {
	return fmt.Sprintf("GET bytes=%d-%d", start, end)
}

func TestPeekFixtureWaitsForHandlerAccounting(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"owned/image:v1"}, layerSize: 500_000})
	rs := &rangeServer{body: archive}
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.ServeHTTP(heldWriteResponse{ResponseWriter: w, release: release}, r)
	}))
	srv.Listener = &closingListener{Listener: srv.Listener, closed: release}
	srv.Start()
	t.Cleanup(srv.Close)

	// The whole response reaches the client, but Write cannot return and update
	// served until the helper closes the listener. No scheduler timing is needed.
	got, err := peekFixture(t, srv)
	if err != nil || !got.Previewable || !slices.Equal(got.RepoTags, []string{"owned/image:v1"}) {
		t.Fatalf("Peek = %+v, err = %v", got, err)
	}
	if served := rs.bytesServed(); served != tailWindow {
		t.Fatalf("handler accounting incomplete: served %d, want %d", served, tailWindow)
	}
}

func TestPeekURLDockerSaveTailOnly(t *testing.T) {
	want := []string{"gpustack/runner:cuda13.0-vllm0.25.1", "gpustack/runner:latest"}
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: want, layerSize: 500_000})
	size := int64(len(archive))
	rs, srv := newRangeServer(t, archive)

	got, err := peekFixture(t, srv)
	if err != nil {
		t.Fatalf("PeekURL: %v", err)
	}
	if !got.Previewable {
		t.Fatalf("Previewable = false, want true")
	}
	if got.SizeBytes != size {
		t.Fatalf("SizeBytes = %d, want %d", got.SizeBytes, size)
	}
	if !slices.Equal(got.RepoTags, want) {
		t.Fatalf("RepoTags = %q, want %q", got.RepoTags, want)
	}
	wantReqs := []string{"HEAD", rangeReq(size-tailWindow, size-1)}
	if !slices.Equal(rs.requests(), wantReqs) {
		t.Fatalf("requests = %q, want %q", rs.requests(), wantReqs)
	}
	if served := rs.bytesServed(); served != tailWindow {
		t.Fatalf("bytes served = %d, want exactly one %d byte tail", served, tailWindow)
	}
}

func TestPeekURLSmallArchiveFetchedWhole(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"tiny/image:v1"}, layerSize: 1024})
	size := int64(len(archive))
	if size >= tailWindow {
		t.Fatalf("fixture is %d bytes, expected smaller than the tail window", size)
	}
	rs, srv := newRangeServer(t, archive)

	got, err := peekFixture(t, srv)
	if err != nil {
		t.Fatalf("PeekURL: %v", err)
	}
	if !got.Previewable || !slices.Equal(got.RepoTags, []string{"tiny/image:v1"}) {
		t.Fatalf("Peek = %+v, want the single tag", got)
	}
	wantReqs := []string{"HEAD", rangeReq(0, size-1)}
	if !slices.Equal(rs.requests(), wantReqs) {
		t.Fatalf("requests = %q, want %q", rs.requests(), wantReqs)
	}
}

func TestPeekURLManifestStraddlesTailWindow(t *testing.T) {
	tags := manyTags(1500)
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: tags, layerSize: 1_200_000})
	size := int64(len(archive))
	if size <= tailWindowLarge {
		t.Fatalf("fixture is %d bytes, expected larger than the 1MiB retry window", size)
	}
	rs, srv := newRangeServer(t, archive)

	got, err := peekFixture(t, srv)
	if err != nil {
		t.Fatalf("PeekURL: %v", err)
	}
	if !got.Previewable {
		t.Fatalf("Previewable = false, want true after the larger tail retry")
	}
	if !slices.Equal(got.RepoTags, tags) {
		t.Fatalf("got %d tags, want %d", len(got.RepoTags), len(tags))
	}
	wantReqs := []string{
		"HEAD",
		rangeReq(size-tailWindow, size-1),
		rangeReq(size-tailWindowLarge, size-1),
	}
	if !slices.Equal(rs.requests(), wantReqs) {
		t.Fatalf("requests = %q, want %q", rs.requests(), wantReqs)
	}
	if served := rs.bytesServed(); served >= maxFetchBytes {
		t.Fatalf("bytes served = %d, want below the %d byte cap", served, int64(maxFetchBytes))
	}
}

func TestPeekURLOCILayoutFallsBackToHead(t *testing.T) {
	want := []string{"gpustack/runner:cuda13.0-sglang0.5", "gpustack/runner:latest"}
	archive := ociArchive(t, want, 2<<20)
	size := int64(len(archive))
	rs, srv := newRangeServer(t, archive)

	got, err := peekFixture(t, srv)
	if err != nil {
		t.Fatalf("PeekURL: %v", err)
	}
	if !got.Previewable || !slices.Equal(got.RepoTags, want) {
		t.Fatalf("Peek = %+v, want %q", got, want)
	}
	if got.SizeBytes != size {
		t.Fatalf("SizeBytes = %d, want %d", got.SizeBytes, size)
	}
	// Worst case: both tail windows, then the head window.
	wantReqs := []string{
		"HEAD",
		rangeReq(size-tailWindow, size-1),
		rangeReq(size-tailWindowLarge, size-1),
		rangeReq(0, headWindow-1),
	}
	if !slices.Equal(rs.requests(), wantReqs) {
		t.Fatalf("requests = %q, want %q", rs.requests(), wantReqs)
	}
	if served := rs.bytesServed(); served >= maxFetchBytes {
		t.Fatalf("bytes served = %d, want below the %d byte cap", served, int64(maxFetchBytes))
	}
}

func TestPeekURLGzipIsNotPreviewable(t *testing.T) {
	plain := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"gz/image:v1"}, layerSize: 300_000})
	archive := gzipBytes(t, append(plain, incompressible(400_000)...))
	size := int64(len(archive))
	if size <= tailWindow {
		t.Fatalf("gzip fixture is only %d bytes", size)
	}
	rs, srv := newRangeServer(t, archive)

	got, err := peekFixture(t, srv)
	if err != nil {
		t.Fatalf("PeekURL: %v", err)
	}
	if got.Previewable || len(got.RepoTags) != 0 {
		t.Fatalf("Peek = %+v, want previewable=false with no tags", got)
	}
	if got.SizeBytes != size {
		t.Fatalf("SizeBytes = %d, want %d", got.SizeBytes, size)
	}
	// No 1MiB retry: a gzip tail holds no tar headers at all.
	wantReqs := []string{"HEAD", rangeReq(size-tailWindow, size-1), rangeReq(0, headWindow-1)}
	if !slices.Equal(rs.requests(), wantReqs) {
		t.Fatalf("requests = %q, want %q", rs.requests(), wantReqs)
	}
}

func TestPeekURLArchiveWithoutManifest(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{omitManifest: true, layerSize: 300_000})
	size := int64(len(archive))
	rs, srv := newRangeServer(t, archive)

	got, err := peekFixture(t, srv)
	if err != nil {
		t.Fatalf("PeekURL: %v", err)
	}
	if got.Previewable || len(got.RepoTags) != 0 {
		t.Fatalf("Peek = %+v, want previewable=false with no tags", got)
	}
	if got.SizeBytes != size {
		t.Fatalf("SizeBytes = %d, want %d", got.SizeBytes, size)
	}
	if served := rs.bytesServed(); served >= maxFetchBytes {
		t.Fatalf("bytes served = %d, want below the %d byte cap", served, int64(maxFetchBytes))
	}
}

func TestPeekURLServerOmitsAcceptRangesButHonorsRange(t *testing.T) {
	want := []string{"quiet/server:v1"}
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: want, layerSize: 500_000})
	size := int64(len(archive))
	rs, srv := newRangeServer(t, archive, withoutAcceptRanges)

	got, err := peekFixture(t, srv)
	if err != nil {
		t.Fatalf("PeekURL: %v", err)
	}
	if !got.Previewable || !slices.Equal(got.RepoTags, want) {
		t.Fatalf("Peek = %+v, want %q", got, want)
	}
	wantReqs := []string{"HEAD", rangeReq(0, probeBytes-1), rangeReq(size-tailWindow, size-1)}
	if !slices.Equal(rs.requests(), wantReqs) {
		t.Fatalf("requests = %q, want a probe then the tail: %q", rs.requests(), wantReqs)
	}
}

func TestPeekURLServerIgnoresRange(t *testing.T) {
	const size = 64 << 20
	fs, srv := newFloodServer(t, size)

	got, err := peekFixture(t, srv)
	if err != nil {
		t.Fatalf("PeekURL: %v", err)
	}
	if got.Previewable || len(got.RepoTags) != 0 {
		t.Fatalf("Peek = %+v, want previewable=false with no tags", got)
	}
	if got.SizeBytes != size {
		t.Fatalf("SizeBytes = %d, want %d", got.SizeBytes, int64(size))
	}
	// The body must be abandoned, not downloaded.
	written := fs.bytesWritten(t)
	t.Logf("server wrote %d of %d body bytes before PeekURL closed the response", written, int64(size))
	if written >= 16<<20 {
		t.Fatalf("server wrote %d bytes of a %d byte body; PeekURL kept reading", written, int64(size))
	}
}

func TestPeekURLHTTPErrors(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"err/image:v1"}})
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name: "not found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			},
			wantErr: "404",
		},
		{
			name: "missing content length",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
			wantErr: "Content-Length",
		},
		{
			name: "head rejected",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				_, _ = w.Write(archive)
			},
			wantErr: "405",
		},
		{
			name: "range returns 500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
				w.Header().Set("Accept-Ranges", "bytes")
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Del("Content-Length")
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErr: "500",
		},
		{
			name: "content range mismatch",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusOK)
					return
				}
				// Answers a different range than the one requested.
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 99-102/%d", len(archive)))
				w.Header().Set("Content-Length", "4")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(archive[99:103])
			},
			wantErr: "Content-Range",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			got, err := peekFixture(t, srv)
			if err == nil {
				t.Fatalf("PeekURL = %+v, want an error", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestPeekURLShortRangeBodyIsNotPreviewable(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"short/image:v1"}, layerSize: 200_000})
	// Honours the range offset but cuts the body short, so the tail never
	// reaches the archive's last byte and nothing can be parsed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		start, end, ok := parseRangeHeader(r.Header.Get("Range"), int64(len(archive)))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		cut := start + (end-start+1)/2
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(archive)))
		w.Header().Set("Content-Length", strconv.FormatInt(cut-start, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(archive[start:cut])
	}))
	t.Cleanup(srv.Close)

	got, err := peekFixture(t, srv)
	if err != nil {
		t.Fatalf("PeekURL: %v", err)
	}
	if got.Previewable || len(got.RepoTags) != 0 {
		t.Fatalf("Peek = %+v, want previewable=false with no tags", got)
	}
	if got.SizeBytes != int64(len(archive)) {
		t.Fatalf("SizeBytes = %d, want %d", got.SizeBytes, len(archive))
	}
}

func TestPeekURLRejectsBadURLs(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"ftp scheme", "ftp://example.com/image.tar"},
		{"file scheme", "file:///tmp/image.tar"},
		{"no scheme", "example.com/image.tar"},
		{"no host", "https:///image.tar"},
		{"embedded credentials", "https://user:secret@example.com/image.tar"},
		{"embedded user", "http://user@example.com/image.tar"},
		{"unparsable", "http://[::1/image.tar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PeekURL(context.Background(), http.DefaultClient, tc.url)
			if !errors.Is(err, ErrInvalidURL) {
				t.Fatalf("err = %v, want ErrInvalidURL", err)
			}
		})
	}
}

func TestPeekURLNilClient(t *testing.T) {
	_, err := PeekURL(context.Background(), nil, "https://example.com/image.tar")
	if err == nil {
		t.Fatalf("PeekURL with a nil client returned no error")
	}
}

func TestPeekURLCanceledContext(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"ctx/image:v1"}})
	_, srv := newRangeServer(t, archive)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PeekURL(ctx, srv.Client(), srv.URL+"/image.tar"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// redirectChain serves /hop/N as a redirect to /hop/N-1 and /hop/0 as the
// archive itself.
func redirectChain(t *testing.T, archive []byte) *httptest.Server {
	t.Helper()
	rs := &rangeServer{body: archive}
	mux := http.NewServeMux()
	mux.HandleFunc("/hop/", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if n == 0 {
			rs.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPeekURLFollowsBoundedRedirects(t *testing.T) {
	want := []string{"redirect/image:v1"}
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: want, layerSize: 200_000})
	srv := redirectChain(t, archive)

	got, err := PeekURL(context.Background(), srv.Client(), srv.URL+"/hop/3")
	if err != nil {
		t.Fatalf("PeekURL over 3 redirects: %v", err)
	}
	if !got.Previewable || !slices.Equal(got.RepoTags, want) {
		t.Fatalf("Peek = %+v, want %q", got, want)
	}

	if _, err := PeekURL(context.Background(), srv.Client(), srv.URL+"/hop/7"); err == nil {
		t.Fatalf("PeekURL over 7 redirects returned no error")
	} else if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("error = %v, want it to mention redirects", err)
	}
}

func TestPeekURLKeepsCallerRedirectPolicy(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"policy/image:v1"}})
	srv := redirectChain(t, archive)

	sentinel := errors.New("caller redirect policy")
	client := &http.Client{
		Transport: srv.Client().Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return sentinel
		},
	}
	_, err := PeekURL(context.Background(), client, srv.URL+"/hop/1")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the caller's redirect policy to win", err)
	}

	plain := srv.Client()
	if _, err := PeekURL(context.Background(), plain, srv.URL+"/hop/1"); err != nil {
		t.Fatalf("PeekURL: %v", err)
	}
	if plain.CheckRedirect != nil {
		t.Fatalf("PeekURL mutated the caller's client")
	}
}
