package webui

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"testing/fstest"
)

// The server gzips a text asset itself, once, for clients that accept it.
func TestAssetIsServedGzippedAndImmutable(t *testing.T) {
	raw := testScript()
	h := testHandler(raw)
	req := httptest.NewRequest(http.MethodGet, "/assets/app-abc12345.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	res := httptest.NewRecorder()

	h.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := res.Header().Get("Cache-Control"); got != immutableAssetCacheControl {
		t.Fatalf("Cache-Control = %q, want %q", got, immutableAssetCacheControl)
	}
	if got := res.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	if got := res.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want JavaScript", got)
	}
	if got, want := res.Header().Get("Content-Length"), len(res.Body.Bytes()); got != strconv.Itoa(want) {
		t.Fatalf("Content-Length = %q, want %d", got, want)
	}

	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatalf("decoded body = %q, want %q", decoded, raw)
	}
}

func TestAssetReturnsIdentityWhenGzipIsNotAccepted(t *testing.T) {
	raw := testScript()
	h := testHandler(raw)
	for _, acceptEncoding := range []string{"", "br", "gzip;q=0, *;q=1"} {
		t.Run(acceptEncoding, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/assets/app-abc12345.js", nil)
			req.Header.Set("Accept-Encoding", acceptEncoding)
			res := httptest.NewRecorder()

			h.ServeHTTP(res, req)

			if got := res.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want identity", got)
			}
			if got := res.Header().Get("Cache-Control"); got != immutableAssetCacheControl {
				t.Fatalf("Cache-Control = %q, want %q", got, immutableAssetCacheControl)
			}
			if !bytes.Equal(res.Body.Bytes(), raw) {
				t.Fatalf("body = %q, want %q", res.Body.Bytes(), raw)
			}
		})
	}
}

// An asset gzip would not shrink, and an image, are served as they are.
func TestAssetGzipDoesNotMakeSmall(t *testing.T) {
	h := handler(fstest.MapFS{
		"index.html":                &fstest.MapFile{Data: []byte("<html>gpuworks</html>")},
		"assets/app-abc12345.js":    &fstest.MapFile{Data: []byte("x")},
		"assets/logo-abc12345.webp": &fstest.MapFile{Data: bytes.Repeat([]byte("w"), 4096)},
	})
	for _, path := range []string{"/assets/app-abc12345.js", "/assets/logo-abc12345.webp"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		res := httptest.NewRecorder()

		h.ServeHTTP(res, req)

		if got := res.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("%s: Content-Encoding = %q, want identity", path, got)
		}
		if got := res.Header().Get("Cache-Control"); got != immutableAssetCacheControl {
			t.Fatalf("%s: Cache-Control = %q", path, got)
		}
	}
}

// Every request after the first is served from the one compressed copy; a
// Range is answered with the whole body, and HEAD with its length alone.
func TestGzippedAssetIsCompressedOnce(t *testing.T) {
	raw := testScript()
	h := testHandler(raw)
	get := func(method, rangeHeader string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/assets/app-abc12345.js", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}
	full := get(http.MethodGet, "")
	if full.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("first request not gzipped: %v", full.Header())
	}
	again := get(http.MethodGet, "")
	if !bytes.Equal(again.Body.Bytes(), full.Body.Bytes()) {
		t.Fatal("the second response differs from the first")
	}
	part := get(http.MethodGet, "bytes=0-9")
	if part.Code != http.StatusOK || !bytes.Equal(part.Body.Bytes(), full.Body.Bytes()) {
		t.Fatalf("range = %d, %d bytes", part.Code, part.Body.Len())
	}
	head := get(http.MethodHead, "")
	if head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(full.Body.Len()) {
		t.Fatalf("HEAD = %d bytes, Content-Length %q", head.Body.Len(), head.Header().Get("Content-Length"))
	}
}

func TestUnversionedAssetIsNotMarkedImmutable(t *testing.T) {
	h := handler(fstest.MapFS{
		"index.html":               &fstest.MapFile{Data: []byte("<html>gpuworks</html>")},
		"assets/empty-models.webp": &fstest.MapFile{Data: []byte("webp")},
	})
	req := httptest.NewRequest(http.MethodGet, "/assets/empty-models.webp", nil)
	res := httptest.NewRecorder()

	h.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("Cache-Control = %q, want no immutable policy", got)
	}
}

func TestMissingAssetIsNotCached(t *testing.T) {
	h := testHandler(testScript())
	req := httptest.NewRequest(http.MethodGet, "/assets/missing-hash.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res := httptest.NewRecorder()

	h.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}
	if got := res.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("Cache-Control = %q, want no cache policy on missing asset", got)
	}
}

func TestIndexAndSPAFallbackRemainUncacheable(t *testing.T) {
	h := handler(fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>gpuworks</html>")},
	})
	for _, path := range []string{"/", "/nodes"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			res := httptest.NewRecorder()

			h.ServeHTTP(res, req)

			if got := res.Header().Get("Cache-Control"); got != "no-cache, no-store, must-revalidate" {
				t.Fatalf("Cache-Control = %q, want HTML no-cache policy", got)
			}
			if got := res.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want identity", got)
			}
		})
	}
}

func testHandler(raw []byte) http.Handler {
	return handler(fstest.MapFS{
		"index.html":             &fstest.MapFile{Data: []byte("<html>gpuworks</html>")},
		"assets/app-abc12345.js": &fstest.MapFile{Data: raw},
	})
}

// testScript is a script large and repetitive enough that gzip shrinks it.
func testScript() []byte {
	return bytes.Repeat([]byte("console.log('gpuworks');\n"), 64)
}
