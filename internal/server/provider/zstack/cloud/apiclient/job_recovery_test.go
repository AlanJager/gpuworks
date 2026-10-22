package apiclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestCloudJobRecoveryAfterLostSubmissionResponseDoesNotReplayWrite(t *testing.T) {
	for _, finalStatus := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusNotFound} {
		t.Run(fmt.Sprint(finalStatus), func(t *testing.T) {
			const jobID = "00112233445566778899aabbccddeeff"
			var writes, queries atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !validTestAccessKey(r) {
					t.Error("job recovery lost AccessKey authentication")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.Method + " " + r.URL.Path {
				case "PUT /v1/pci-device/pci-devices/gpu/actions":
					if r.Header.Get("X-Job-UUID") != jobID || writes.Add(1) != 1 {
						t.Error("mutation was not correlated or was replayed")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					// Cloud accepted the job, but the connection failed before
					// the client received the 202 response and its location.
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				case "GET /v1/api-jobs/" + jobID:
					if queries.Add(1) == 1 {
						w.WriteHeader(http.StatusAccepted)
						fmt.Fprint(w, `{}`)
						return
					}
					w.WriteHeader(finalStatus)
					switch finalStatus {
					case http.StatusOK:
						fmt.Fprint(w, `{}`)
					case http.StatusServiceUnavailable:
						fmt.Fprint(w, `{"error":{"code":"SYS.1000","details":"operation rejected"}}`)
					default:
						fmt.Fprint(w, `{"error":{"code":"SYS.1003","details":"job result expired"}}`)
					}
				default:
					t.Errorf("unexpected recovery request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			newClient := func() *APIClient {
				c, err := New(provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}, WithPollInterval(time.Millisecond))
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			payload := map[string]any{"updatePciDevice": map[string]string{"state": "Disabled"}}
			err := newClient().PutActionWithJob(t.Context(), "/pci-device/pci-devices/gpu/actions", payload, jobID)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || !apiErr.OutcomeUnknown || apiErr.JobID != jobID {
				t.Fatalf("lost submission response = %v, want recoverable unknown outcome", err)
			}
			// A newly constructed client needs only the persisted job ID.
			err = newClient().WaitForJob(t.Context(), jobID)
			if finalStatus == http.StatusOK {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.As(err, &apiErr) || apiErr.JobID != jobID || apiErr.OutcomeUnknown != (finalStatus == http.StatusNotFound) {
				t.Fatalf("recovered HTTP %d outcome = %v", finalStatus, err)
			}
			if writes.Load() != 1 || queries.Load() != 2 {
				t.Fatalf("recovery replayed a write or ignored the pending job: writes=%d queries=%d", writes.Load(), queries.Load())
			}
		})
	}
}

func TestCloudJobIdentityValidationDoesNotSubmit(t *testing.T) {
	c := clientForTest(t)
	c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("invalid job identity reached Cloud: %s", r.URL)
		return cloudResponse(r, http.StatusOK, `{}`), nil
	})
	for _, id := range []string{"", "../job", "00112233-4455-6677-8899-aabbccddeeff", "00112233445566778899aabbccddeefg"} {
		if err := c.PutActionWithJob(t.Context(), "/actions", map[string]any{}, id); err == nil {
			t.Errorf("accepted invalid submission identity %q", id)
		}
		if err := c.WaitForJob(t.Context(), id); err == nil {
			t.Errorf("accepted invalid lookup identity %q", id)
		}
	}
}
