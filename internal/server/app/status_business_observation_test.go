//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatusBusinessObservationExplainsUnconfirmedReadiness(t *testing.T) {
	database, stop := openStatusPort(t)
	t.Cleanup(stop)
	root := writeStatusRaft(t, statusIdentity(7, database, 0), statusMembers(statusIdentity(7, database, 0)), map[string]string{"metadata1": "m", "0000000000000001-0000000000000001": "s"})
	for _, tc := range []struct {
		name, business, reason string
	}{
		{"missing-listener", "not observed", "readyz skipped: HTTP listener not listening"},
		{"timeout", "not observed", "readyz request timed out"},
		{"transport", "not observed", "readyz request transport failed"},
		{"short-body", "not observed", "readyz response body read failed"},
		{"body-timeout", "not observed", "readyz response body timed out"},
		{"oversize", "not observed", "readyz response body exceeds 512 bytes"},
		{"malformed", "invalid readiness response", "readyz response does not match the readiness contract"},
		{"rejected", "member business service is not ready", ""},
		{"ready", "open", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/readyz" || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				switch tc.name {
				case "timeout":
					<-r.Context().Done()
					return
				case "transport":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				case "rejected":
					http.Error(w, "member business service is not ready", http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				if tc.name == "short-body" {
					w.Header().Set("Content-Length", "100")
				}
				if tc.name == "body-timeout" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				body := `{"status":"ready"}`
				if tc.name == "malformed" {
					body = `{"secret":"never-print-this"}`
				}
				if tc.name == "oversize" {
					body = strings.Repeat("never-print-this", 40)
				}
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)
			addr := server.Listener.Addr().String()
			if tc.name == "missing-listener" {
				server.Close()
			}
			report, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, addr, 100*time.Millisecond)
			if tc.name == "ready" {
				if err != nil || !strings.Contains(report, "Overall: ready (business reads verified)\n") {
					t.Fatalf("ready report: %v\n%s", err, report)
				}
			} else if !errors.Is(err, ErrBusinessStatusNotReady) || !strings.Contains(report, "Overall: not ready (") {
				t.Fatalf("unconfirmed report: %v\n%s", err, report)
			}
			if !strings.Contains(report, "Business: "+tc.business+"\n") {
				t.Fatalf("business classification changed:\n%s", report)
			}
			if tc.reason != "" && !strings.Contains(report, "Business observation: "+tc.reason+"\n") {
				t.Fatalf("missing diagnostic %q:\n%s", tc.reason, report)
			}
			if tc.reason == "" && strings.Contains(report, "Business observation:") {
				t.Fatalf("unexpected diagnostic:\n%s", report)
			}
			if strings.Contains(report, "never-print-this") {
				t.Fatal("response body leaked into report")
			}
		})
	}
}

func TestStatusBusinessObservationSeparatesCancellationAndSanitizesRequestError(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		select {
		case <-entered:
			cancel()
		case <-ctx.Done():
		}
	}()
	classification, reason := observeBusiness(ctx, server.Listener.Addr().String(), time.Second)
	if classification != "not observed" || reason != "readyz request canceled" {
		t.Fatalf("canceled request: %q, %q", classification, reason)
	}
	classification, reason = observeBusiness(t.Context(), "user:never-print-this@%zz", time.Second)
	if classification != "not observed" || reason != "readyz request could not be constructed" {
		t.Fatalf("invalid request: %q, %q", classification, reason)
	}
}
