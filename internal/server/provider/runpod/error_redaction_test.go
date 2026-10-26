package runpod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestUnsuccessfulResponsesDoNotExposeUpstreamBodies(t *testing.T) {
	const secret = "synthetic-runpod-bearer-key"
	for _, test := range []struct {
		name, method, path, operation string
		call                          func(*Driver, provider.Credentials) error
	}{
		{"list", "GET", "/pods", "list pods", func(d *Driver, c provider.Credentials) error { _, err := d.ListInstances(t.Context(), c); return err }},
		{"create", "POST", "/pods", "create pod", func(d *Driver, c provider.Credentials) error {
			_, err := d.CreateInstance(t.Context(), c, provider.CreateInstanceRequest{GPUModel: "fixture-gpu"})
			return err
		}},
		{"delete", "DELETE", "/pods/fixture", "delete pod", func(d *Driver, c provider.Credentials) error { return d.DeleteInstance(t.Context(), c, "fixture") }},
		{"edit", "PATCH", "/pods/fixture", "edit pod", func(d *Driver, c provider.Credentials) error {
			_, err := d.editPod(t.Context(), c, pod{ID: "fixture"}, map[string]any{"ports": []string{"22/tcp"}}, "fixture", func(pod) error { t.Fatal("failed edit must not poll readiness"); return nil })
			return err
		}},
		{"list-keys", "GET", "/account/ssh-keys", "list SSH keys", func(d *Driver, c provider.Credentials) error { _, err := d.ListSSHKeys(t.Context(), c); return err }},
		{"replace-keys", "PUT", "/account/ssh-keys", "replace SSH keys", func(d *Driver, c provider.Credentials) error {
			return d.ReplaceSSHKeys(t.Context(), c, []string{"ssh-ed25519 AAAA fixture"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			driver := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != test.method || r.URL.Path != test.path || r.Header.Get("Authorization") != "Bearer "+secret {
					t.Errorf("unexpected request method/path/authentication")
				}
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":"vendor diagnostic: Bearer %s"}`, secret)
			})
			err := test.call(driver, provider.Credentials{APIKey: secret})
			if err == nil || !strings.Contains(err.Error(), test.operation) || !strings.Contains(err.Error(), "status 400") {
				t.Fatalf("operation/status missing: %v", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "vendor diagnostic") {
				t.Fatalf("untrusted upstream body in error: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("HTTP status failures must not retry: got %d requests", calls.Load())
			}
		})
	}
}

func TestCanceledListingRetainsContextError(t *testing.T) {
	driver := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) { t.Error("canceled request reached upstream") })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := driver.ListInstances(ctx, provider.Credentials{APIKey: "synthetic"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation identity lost: %v", err)
	}
}
