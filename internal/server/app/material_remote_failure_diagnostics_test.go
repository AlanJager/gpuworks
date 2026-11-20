//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRemoteMaterialBatchMetadataDiagnosticsKeepRejection(t *testing.T) {
	for _, mode := range []string{"status", "content_type", "cache_control", "member", "length"} {
		t.Run(mode, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			files := []materialBatchItem{f.put([]byte("DO-NOT-LOG-material-bytes"))}
			expectedLength, err := materialBatchLength(files)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
				actual := httptest.NewRecorder()
				f.handler.ServeHTTP(actual, r)
				if actual.Code != http.StatusOK {
					t.Errorf("real authenticated batch handler status=%d", actual.Code)
				}
				for key, values := range actual.Header() {
					w.Header()[key] = values
				}
				w.Header().Set("Authorization", "Bearer DO-NOT-LOG-response-header")
				status := actual.Code
				switch mode {
				case "status":
					status = http.StatusUnauthorized
				case "content_type":
					w.Header().Set("Content-Type", "DO-NOT-LOG-content-type")
				case "cache_control":
					w.Header().Set("Cache-Control", "DO-NOT-LOG-cache-control")
				case "member":
					w.Header().Set("X-Gpuworks-Material-Member", "DO-NOT-LOG-member-header")
				case "length":
					w.Header().Set("Content-Length", strconv.FormatInt(expectedLength+1, 10))
				}
				w.WriteHeader(status)
				_, _ = io.Copy(w, bytes.NewReader(actual.Body.Bytes()))
				return true
			}
			f.mu.Unlock()
			_, err = verifyRemoteMaterialBatchLimit(t.Context(), f.client, f.server.URL, files, 7)
			if err == nil {
				t.Fatal("bad real HTTP metadata became accepted")
			}
			expectedStatus := 200
			if mode == "status" {
				expectedStatus = 401
			}
			expectedActualLength := expectedLength
			if mode == "length" {
				expectedActualLength++
			}
			for _, part := range []string{
				"remote material batch response metadata differs",
				"status=" + strconv.Itoa(expectedStatus),
				"content_type_match=" + strconv.FormatBool(mode != "content_type"),
				"cache_control_match=" + strconv.FormatBool(mode != "cache_control"),
				"member_match=" + strconv.FormatBool(mode != "member"),
				"content_length=" + strconv.FormatInt(expectedActualLength, 10),
				"expected_length=" + strconv.FormatInt(expectedLength, 10),
			} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("original failure missing bounded diagnostic %q: %v", part, err)
				}
			}
			if strings.Contains(err.Error(), "DO-NOT-LOG") {
				t.Fatalf("raw header/material data leaked: %v", err)
			}
		})
	}
}

func TestRemoteMaterialBatchDiagnosticsKeepUnavailableAndCancellation(t *testing.T) {
	for _, mode := range []string{"unavailable", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			files := []materialBatchItem{f.put([]byte("actual authenticated fixture bytes"))}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.mu.Lock()
			f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
				if mode == "canceled" {
					cancel()
					select {
					case <-r.Context().Done():
					case <-time.After(2 * time.Second):
					}
				} else {
					http.Error(w, "DO-NOT-LOG-unavailable-body", http.StatusServiceUnavailable)
				}
				return true
			}
			f.mu.Unlock()
			_, err := verifyRemoteMaterialBatchLimit(ctx, f.client, f.server.URL, files, 7)
			expected := errRemoteMaterialBatchUnavailable
			if mode == "canceled" {
				expected = context.Canceled
			}
			if !errors.Is(err, expected) {
				t.Fatalf("error classification changed: %T %v want%v", err, err, expected)
			}
			if err != nil && strings.Contains(err.Error(), "DO-NOT-LOG") {
				t.Fatal("response body leaked", err)
			}
		})
	}
}
