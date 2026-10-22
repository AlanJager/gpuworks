// Package webui embeds the built React SPA and serves it with SPA-style
// fallback (unknown non-asset paths return index.html so client routing works).
// Vite is configured to build into this package's dist/ directory.
package webui

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const immutableAssetCacheControl = "public, max-age=31536000, immutable"

var contentHashedAsset = regexp.MustCompile(`(?:^|/)[^/]+-[A-Za-z0-9_-]{8}\.[^./]+$`)

//go:embed all:dist
var embedded embed.FS

// Handler returns an http.Handler serving the embedded SPA.
func Handler() http.Handler {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err)
	}
	return handler(sub)
}

func handler(sub fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(sub))
	gz := &gzipCache{sub: sub, entries: map[string]*gzipEntry{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		// The HTML entrypoint contains a content-hashed asset name. Do not let a
		// browser keep an old entrypoint across a deployment, otherwise it can
		// request an asset that is no longer embedded by the new server binary.
		if p == "index.html" {
			serveIndex(sub, w, r)
			return
		}
		// Serve real files (assets); otherwise fall back to index.html so the
		// history-mode router can take over.
		if _, err := fs.Stat(sub, p); err != nil {
			if !strings.HasPrefix(p, "assets/") {
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/"
				serveIndex(sub, w, r2)
				return
			}
		}
		if strings.HasPrefix(p, "assets/") {
			serveAsset(sub, fileServer, gz, p, w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func serveAsset(sub fs.FS, fileServer http.Handler, gz *gzipCache, p string, w http.ResponseWriter, r *http.Request) {
	if _, err := fs.Stat(sub, p); err != nil {
		fileServer.ServeHTTP(w, r)
		return
	}

	// Vite content hashes make these URLs safe to retain across deployments.
	// Files copied verbatim from public/assets have stable names and must not be
	// marked immutable, otherwise a replacement image could stay stale for a year.
	if contentHashedAsset.MatchString(p) {
		w.Header().Set("Cache-Control", immutableAssetCacheControl)
	}
	if !compressible[extname(p)] {
		fileServer.ServeHTTP(w, r)
		return
	}
	w.Header().Add("Vary", "Accept-Encoding")
	if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
		fileServer.ServeHTTP(w, r)
		return
	}
	compressed := gz.get(p)
	if compressed == nil {
		fileServer.ServeHTTP(w, r)
		return
	}
	if contentType := mime.TypeByExtension(extname(p)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	// Written whole: a Range on an encoded representation buys a browser
	// nothing here, and HTTP lets a server answer it with the full body.
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(compressed)
	}
}

// compressible are the text formats worth gzipping; images and fonts arrive
// compressed already.
var compressible = map[string]bool{".css": true, ".html": true, ".js": true, ".json": true, ".svg": true}

// gzipCache holds the gzipped form of each text asset, made the first time a
// client asks for it and kept for the life of the process: the embedded files
// never change, so each costs one compression, not one per request, and the
// binary need not carry a second, already-compressed copy of the UI.
type gzipCache struct {
	sub     fs.FS
	mu      sync.Mutex
	entries map[string]*gzipEntry
}

type gzipEntry struct {
	once sync.Once
	data []byte // nil when gzip would not make the asset smaller
}

func (c *gzipCache) get(p string) []byte {
	c.mu.Lock()
	e := c.entries[p]
	if e == nil {
		e = &gzipEntry{}
		c.entries[p] = e
	}
	c.mu.Unlock()
	e.once.Do(func() {
		raw, err := fs.ReadFile(c.sub, p)
		if err != nil {
			return
		}
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
		if _, err := zw.Write(raw); err != nil || zw.Close() != nil {
			return
		}
		if b.Len() < len(raw) {
			e.data = b.Bytes()
		}
	})
	return e.data
}

func extname(path string) string {
	if dot := strings.LastIndexByte(path, '.'); dot >= 0 {
		return path[dot:]
	}
	return ""
}

func acceptsGzip(header string) bool {
	wildcardQuality := 0.0
	for _, value := range strings.Split(header, ",") {
		parts := strings.Split(value, ";")
		encoding := strings.ToLower(strings.TrimSpace(parts[0]))
		quality := 1.0
		for _, param := range parts[1:] {
			key, raw, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil || parsed < 0 || parsed > 1 {
				quality = 0
			} else {
				quality = parsed
			}
		}
		switch encoding {
		case "gzip":
			return quality > 0
		case "*":
			wildcardQuality = quality
		}
	}
	return wildcardQuality > 0
}

func serveIndex(sub fs.FS, w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "ui not built", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_, _ = w.Write(b)
}
