package ocifixture

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fixedTime keeps every generated archive byte-identical across runs.
var fixedTime = time.Unix(1_700_000_000, 0).UTC()

// pseudoRandom returns n deterministic bytes that do not compress, so a
// fixture archive's size is the size the test asked for.
func pseudoRandom(n int) []byte {
	out := make([]byte, n)
	state := uint64(0x9E3779B97F4A7C15)
	for i := range out {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		out[i] = byte(state)
	}
	return out
}

func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DockerSaveOptions describes a synthetic `docker save` archive.
type DockerSaveOptions struct {
	// RepoTags are the references baked into manifest.json. An empty list
	// produces an archive whose references cannot be previewed.
	RepoTags []string
	// LayerBytes pads the layer payload so the archive is larger than the tail
	// window a previewer reads. Zero uses a small default.
	LayerBytes int
	// OmitManifest drops manifest.json and the legacy repositories entry,
	// producing an archive no previewer can name.
	OmitManifest bool
}

// DockerSaveArchive builds a deterministic archive in the layout `docker save`
// writes: the image config blob, one legacy layer directory, then manifest.json
// and the legacy repositories entry at the very end, followed by the two zero
// blocks every tar ends with. Tests that need a real archive but cannot run
// docker use this; the tail layout is what makes a range-based preview
// possible, so it is reproduced exactly rather than approximated.
func DockerSaveArchive(opts DockerSaveOptions) ([]byte, error) {
	if opts.LayerBytes <= 0 {
		opts.LayerBytes = 4096
	}
	// `docker save` stores layer.tar uncompressed, so the fixture does too.
	// The filler is deterministic but incompressible, which keeps LayerBytes a
	// truthful handle on the archive's size for range and progress tests.
	layer, err := buildSaveTar([]saveEntry{
		{name: "etc/gpuworks-fixture", data: []byte("fixture\n")},
		{name: "usr/share/blob.bin", data: pseudoRandom(opts.LayerBytes)},
	})
	if err != nil {
		return nil, fmt.Errorf("build layer: %w", err)
	}
	layerDir := hexDigest(layer)
	// Like docker, the config names the uncompressed layer by its digest
	// (diff_id); loaders verify layer.tar against it.
	config := []byte(`{"architecture":"amd64","os":"linux","config":{"Entrypoint":["/entrypoint.sh"],"Cmd":["--serve"],"Env":["PATH=/usr/bin"]},"rootfs":{"type":"layers","diff_ids":["sha256:` + layerDir + `"]}}`)
	configName := hexDigest(config) + ".json"

	entries := []saveEntry{
		{name: configName, data: config},
		{name: layerDir + "/", dir: true},
		{name: layerDir + "/VERSION", data: []byte("1.0")},
		{name: layerDir + "/json", data: []byte(`{"id":"` + layerDir + `"}`)},
		{name: layerDir + "/layer.tar", data: layer},
	}
	if !opts.OmitManifest {
		manifestJSON, err := json.Marshal([]map[string]any{{
			"Config":   configName,
			"RepoTags": opts.RepoTags,
			"Layers":   []string{layerDir + "/layer.tar"},
		}})
		if err != nil {
			return nil, fmt.Errorf("marshal manifest: %w", err)
		}
		repositories := map[string]map[string]string{}
		for _, tag := range opts.RepoTags {
			name, ref, ok := strings.Cut(tag, ":")
			if !ok {
				continue
			}
			if repositories[name] == nil {
				repositories[name] = map[string]string{}
			}
			repositories[name][ref] = layerDir
		}
		repoJSON, err := json.Marshal(repositories)
		if err != nil {
			return nil, fmt.Errorf("marshal repositories: %w", err)
		}
		entries = append(entries,
			saveEntry{name: "manifest.json", data: manifestJSON},
			saveEntry{name: "repositories", data: repoJSON},
		)
	}
	return buildSaveTar(entries)
}

type saveEntry struct {
	name string
	dir  bool
	data []byte
}

func buildSaveTar(entries []saveEntry) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data)), ModTime: fixedTime, Format: tar.FormatUSTAR, Typeflag: tar.TypeReg}
		if e.dir {
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if !e.dir {
			if _, err := tw.Write(e.data); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ArchiveServer is a faithful static file server for an image archive: HEAD
// reports Content-Length and Accept-Ranges, a ranged GET answers 206 with a
// correct Content-Range, and an unranged GET answers 200 with the whole body.
// It records every request so a test can prove which ones were made.
type ArchiveServer struct {
	body           []byte
	noAcceptRanges bool
	breakAfter     int64

	mu       sync.Mutex
	requests []string
}

// ArchiveServerOption adjusts the fixture's behaviour.
type ArchiveServerOption func(*ArchiveServer)

// WithoutRangeSupport makes the server advertise no Accept-Ranges and ignore
// every Range header, answering 200 with the whole body.
func WithoutRangeSupport() ArchiveServerOption {
	return func(s *ArchiveServer) { s.noAcceptRanges = true }
}

// WithMidStreamBreak closes the response after n body bytes, reproducing a
// mirror that drops a large transfer halfway through.
func WithMidStreamBreak(n int64) ArchiveServerOption {
	return func(s *ArchiveServer) { s.breakAfter = n }
}

// StartArchiveServer serves body at "/image.tar" until the test ends.
func StartArchiveServer(body []byte, opts ...ArchiveServerOption) (*ArchiveServer, *httptest.Server) {
	s := &ArchiveServer{body: body}
	for _, opt := range opts {
		opt(s)
	}
	return s, httptest.NewServer(s)
}

// Requests returns the recorded "METHOD path Range-header" lines.
func (s *ArchiveServer) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *ArchiveServer) record(line string) {
	s.mu.Lock()
	s.requests = append(s.requests, strings.TrimSpace(line))
	s.mu.Unlock()
}

func (s *ArchiveServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	total := int64(len(s.body))
	rangeHdr := r.Header.Get("Range")
	s.record(r.Method + " " + r.URL.Path + " " + rangeHdr)
	if r.URL.Path != "/image.tar" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/x-tar")
	h.Set("ETag", `"gpuworks-fixture"`)
	if !s.noAcceptRanges {
		h.Set("Accept-Ranges", "bytes")
	}
	switch r.Method {
	case http.MethodHead:
		h.Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		start, end := int64(0), total-1
		status := http.StatusOK
		if rangeHdr != "" && !s.noAcceptRanges {
			parsed, parsedEnd, ok := parseByteRange(rangeHdr, total)
			if !ok {
				h.Set("Content-Range", fmt.Sprintf("bytes */%d", total))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start, end, status = parsed, parsedEnd, http.StatusPartialContent
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		}
		payload := s.body[start : end+1]
		if s.breakAfter > 0 && int64(len(payload)) > s.breakAfter {
			// Announce the full length, then stop early: the client sees a
			// truncated body rather than a clean short response.
			h.Set("Content-Length", strconv.FormatInt(int64(len(payload)), 10))
			w.WriteHeader(status)
			_, _ = w.Write(payload[:s.breakAfter])
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		h.Set("Content-Length", strconv.FormatInt(int64(len(payload)), 10))
		w.WriteHeader(status)
		_, _ = w.Write(payload)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// parseByteRange understands the single "bytes=start-end", "bytes=start-" and
// "bytes=-suffix" forms the downloader and the previewer emit.
func parseByteRange(v string, total int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(v), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > total {
			n = total
		}
		return total - n, total - 1, true
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= total {
		return 0, 0, false
	}
	end = total - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end > total-1 {
			end = total - 1
		}
	}
	return start, end, true
}
