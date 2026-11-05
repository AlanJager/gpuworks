package api

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
)

// storageTransferRequestID exposes the same correlation key the router and
// access log use. It is diagnostic context, never an authorization decision.
func storageTransferRequestID(w http.ResponseWriter, r *http.Request) string {
	id := middleware.GetReqID(r.Context())
	if id != "" {
		w.Header().Set("X-Request-ID", id)
	}
	return id
}

type storageDownloadReader struct {
	r   io.Reader
	err error
}

func (r *storageDownloadReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

type storageDownloadWriter struct {
	w   io.Writer
	err error
}

func (w *storageDownloadWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

// copyStorageDownload records which I/O operation actually failed, without
// buffering the file or inferring a network root cause from an error string.
func copyStorageDownload(dst io.Writer, src io.Reader, expected int64) (int64, string, string, error) {
	r := &storageDownloadReader{r: src}
	w := &storageDownloadWriter{w: dst}
	n, err := io.Copy(w, r)
	switch {
	case w.err != nil:
		return n, "client_write", "gateway_to_client", err
	case r.err != nil:
		return n, "source_read", "node_to_gateway", err
	case err != nil:
		return n, "copy", "unknown", err
	case expected >= 0 && n != expected:
		return n, "response_length", "node_to_gateway", io.ErrUnexpectedEOF
	default:
		return n, "", "", nil
	}
}

func storageTransferErrorDetail(err error, nodeToken string) string {
	return diagnostics.RedactErrorText(err.Error(), 1024, nodeToken)
}
