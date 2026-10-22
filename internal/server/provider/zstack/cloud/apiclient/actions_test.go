package apiclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestCloudActionWaitsForFinalJobResult(t *testing.T) {
	for _, finalStatus := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(finalStatus), func(t *testing.T) {
			polled, finish := make(chan struct{}), make(chan struct{}, 1)
			var queries atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !validTestAccessKey(r) {
					t.Error("job request lost AccessKey authentication")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.Method + " " + r.URL.Path {
				case "PUT /v1/pci-device/pci-devices/gpu/actions":
					w.WriteHeader(http.StatusAccepted)
					fmt.Fprint(w, `{"location":"http://internal-management.invalid/zstack/v1/api-jobs/write","apiTimeout":30000}`)
				case "GET /v1/api-jobs/write":
					if queries.Add(1) == 1 {
						w.WriteHeader(http.StatusAccepted)
						fmt.Fprint(w, `{}`)
						return
					}
					close(polled)
					select {
					case <-finish:
					case <-r.Context().Done():
						return
					}
					w.WriteHeader(finalStatus)
					if finalStatus == http.StatusOK {
						fmt.Fprint(w, `{}`)
					} else {
						fmt.Fprint(w, `{"error":{"code":"write-rejected"}}`)
					}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			defer close(finish)
			client, err := New(provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				result <- client.PutAction(ctx, "/pci-device/pci-devices/gpu/actions", map[string]any{"updatePciDevice": map[string]string{"state": "Disabled"}})
			}()
			select {
			case err := <-result:
				t.Fatalf("returned before final job result: %v", err)
			case <-polled:
			case <-ctx.Done():
				t.Fatal("job was not polled on the configured endpoint")
			}
			select {
			case err := <-result:
				t.Fatalf("accepted request was treated as completed: %v", err)
			default:
			}
			finish <- struct{}{}
			err = <-result
			if (err != nil) != (finalStatus != http.StatusOK) {
				t.Fatalf("final HTTP %d: err=%v", finalStatus, err)
			}
		})
	}
}

func TestCloudActionPendingJobHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"location":"/v1/api-jobs/pending"}`)
		if r.Method == http.MethodGet {
			cancel()
		}
	}))
	defer server.Close()
	client, err := New(provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PutAction(ctx, "/pci-device/pci-devices/gpu/actions", map[string]string{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pending job ignored cancellation: %v", err)
	}
}

func TestCloudActionRejectsInvalidJobLocation(t *testing.T) {
	for _, location := range []string{"", "/not-a-job/id", "/v1/api-jobs/", "/v1/api-jobs/../other", "/v1/api-jobs/id?token=secret"} {
		if path, err := actionJobPath(location); err == nil {
			t.Errorf("accepted invalid location %q as %q", location, path)
		}
	}
}
