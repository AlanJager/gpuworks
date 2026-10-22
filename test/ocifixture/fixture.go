// Package ocifixture builds deterministic synthetic OCI/Docker v2 images for
// runtime integration tests. It deliberately supports filesystem edge cases
// that ordinary Dockerfiles cannot express portably: hard links, symbolic
// links, whiteouts, opaque directories, and padded large layers.
package ocifixture

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"time"
)

// Entry is one filesystem member in a layer. A plain entry uses Body; exactly
// one of Dir, Symlink, or Hardlink may instead describe its type. Whiteouts are
// ordinary empty entries named .wh.<target>; opaque markers use .wh..wh..opq.
type Entry struct {
	Path     string
	Body     []byte
	Dir      bool
	Symlink  string
	Hardlink string
	Mode     int64
	PadBytes int64
}

// BuildLayer returns one gzip-compressed layer tar.
func BuildLayer(entries []Entry) ([]byte, error) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, entry := range entries {
		mode := entry.Mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(entry.Body)) + entry.PadBytes
		header := &tar.Header{Name: entry.Path, Mode: mode, Size: size, Typeflag: tar.TypeReg}
		switch {
		case entry.Dir:
			header.Typeflag, header.Mode, header.Size = tar.TypeDir, 0o755, 0
		case entry.Symlink != "":
			header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, entry.Symlink, 0
		case entry.Hardlink != "":
			header.Typeflag, header.Linkname, header.Size = tar.TypeLink, entry.Hardlink, 0
		}
		if err := tw.WriteHeader(header); err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := tw.Write(entry.Body); err != nil {
				return nil, err
			}
			if entry.PadBytes > 0 {
				if _, err := tw.Write(make([]byte, entry.PadBytes)); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

// TLSRegistry serves one immutable image from an in-process TLS registry.
type TLSRegistry struct {
	Server *httptest.Server
	Image  string
	CAPath string
}

// Close releases the listener and generated CA file.
func (r *TLSRegistry) Close() {
	if r == nil {
		return
	}
	if r.Server != nil {
		r.Server.Close()
	}
	_ = os.Remove(r.CAPath)
}

// StartTLSRegistry exposes layers as gpuworks/harness pinned by manifest
// digest, with entrypoint and env as the image config's Entrypoint and Env.
// Blob responses support Range requests through http.ServeContent.
func StartTLSRegistry(layers [][]byte, entrypoint, env []string) (*TLSRegistry, error) {
	blobs := make(map[string][]byte)
	describe := func(data []byte, mediaType string) descriptor {
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		blobs[digest] = data
		return descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(data))}
	}
	config, err := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH, "os": "linux",
		"config": map[string]any{"Entrypoint": entrypoint, "Env": env},
	})
	if err != nil {
		return nil, err
	}
	m := manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.docker.distribution.manifest.v2+json",
		Config:        describe(config, "application/vnd.docker.container.image.v1+json"),
	}
	for _, layer := range layers {
		m.Layers = append(m.Layers, describe(layer, "application/vnd.docker.image.rootfs.diff.tar.gzip"))
	}
	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	manifestSum := sha256.Sum256(manifestJSON)
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, request *http.Request) {
		path := strings.TrimPrefix(request.URL.Path, "/v2/")
		switch {
		case path == "":
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(path, "gpuworks/harness/manifests/"):
			w.Header().Set("Content-Type", m.MediaType)
			_, _ = w.Write(manifestJSON)
		case strings.HasPrefix(path, "gpuworks/harness/blobs/"):
			digest := strings.TrimPrefix(path, "gpuworks/harness/blobs/")
			blob, ok := blobs[digest]
			if !ok {
				http.NotFound(w, request)
				return
			}
			http.ServeContent(w, request, "", time.Time{}, bytes.NewReader(blob))
		default:
			http.NotFound(w, request)
		}
	})
	server := httptest.NewTLSServer(mux)
	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		server.Close()
		return nil, err
	}
	caFile, err := os.CreateTemp("", "gpuworks-registry-ca-*.pem")
	if err != nil {
		server.Close()
		return nil, err
	}
	if err := pem.Encode(caFile, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
		server.Close()
		_ = caFile.Close()
		return nil, err
	}
	if err := caFile.Close(); err != nil {
		server.Close()
		return nil, err
	}
	host := strings.TrimPrefix(server.URL, "https://")
	return &TLSRegistry{
		Server: server,
		Image:  fmt.Sprintf("%s/gpuworks/harness@sha256:%x", host, manifestSum),
		CAPath: caFile.Name(),
	}, nil
}
