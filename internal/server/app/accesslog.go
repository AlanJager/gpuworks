package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

const maxAccessLogBody = 64 << 10

var accessLogSensitiveFields = map[string]struct{}{
	"authorization": {}, "cookie": {}, "credentials": {}, "env": {},
	"extra_args": {}, "docker_args": {}, "pre_start_script": {},
}

// accessLogHandler emits one completion record for ordinary requests. Long
// lived SSE and WebSocket requests additionally get an opening record so an
// active connection is visible before it eventually closes.
func accessLogHandler(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := "r" + strconv.FormatUint(middleware.NextRequestID(), 10)
		r.Header.Set("X-Request-ID", requestID)
		params := accessLogParams(r)
		stream := isStreamingRequest(r)
		if stream {
			log.Info("http stream opened", "request_id", requestID, "method", r.Method,
				"path", r.URL.Path, "remote", r.RemoteAddr)
		}

		rw := &accessResponseWriter{ResponseWriter: w}
		defer func() {
			message := "http request"
			if stream {
				message = "http stream closed"
			}
			attrs := []any{"request_id", requestID, "method", r.Method, "path", r.URL.Path,
				"status", rw.statusCode(), "bytes", rw.bytes, "duration_ms", time.Since(started).Seconds() * 1000}
			if params != "" {
				attrs = append(attrs, "params", params)
			}
			log.Info(message, attrs...)
		}()
		next.ServeHTTP(rw, r)
	})
}

func accessLogParams(r *http.Request) string {
	if r.Body == nil || (r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch) {
		return ""
	}
	// An upload's body is file content, whatever type the browser labels it
	// with: never read ahead of the handler, never logged.
	if isStorageFileTransfer(r) {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return ""
	}
	original := r.Body
	prefix, err := io.ReadAll(io.LimitReader(original, maxAccessLogBody+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(prefix), original), Closer: original}
	if err != nil || len(prefix) > maxAccessLogBody {
		return ""
	}
	var value any
	if json.Unmarshal(prefix, &value) != nil {
		return ""
	}
	redactAccessLogValue(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func redactAccessLogValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if sensitiveAccessLogField(key) {
				typed[key] = "[REDACTED]"
				continue
			}
			redactAccessLogValue(child)
		}
	case []any:
		for _, child := range typed {
			redactAccessLogValue(child)
		}
	}
}

func sensitiveAccessLogField(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	if _, ok := accessLogSensitiveFields[normalized]; ok {
		return true
	}
	return strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "private_key") ||
		strings.Contains(normalized, "api_key") ||
		strings.Contains(normalized, "credential") ||
		normalized == "token" || strings.HasSuffix(normalized, "_token")
}

func isStreamingRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") ||
		r.URL.Path == "/api/v1/events" ||
		(strings.HasPrefix(r.URL.Path, "/api/v1/instances/") && strings.HasSuffix(r.URL.Path, "/logs")) ||
		isStorageFileTransfer(r)
}

// isStorageFileTransfer matches a storage-files upload or download (ADR
// 0053): a transfer of arbitrary length, logged when it opens as well as when
// it ends.
func isStorageFileTransfer(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasPrefix(p, "/api/v1/storages/") &&
		(strings.HasSuffix(p, "/files/upload") || strings.HasSuffix(p, "/files/download"))
}

type accessResponseWriter struct {
	http.ResponseWriter
	status  int
	bytes   int64
	written bool
}

func (w *accessResponseWriter) WriteHeader(code int) {
	if w.written {
		return
	}
	w.status = code
	w.written = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessResponseWriter) Write(p []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *accessResponseWriter) Flush() {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *accessResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("http hijacking is not supported")
	}
	return h.Hijack()
}

func (w *accessResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *accessResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
