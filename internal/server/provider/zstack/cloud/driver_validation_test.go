package cloud

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
)

func TestCloudValidationUsesSignedAPIWithoutPreflightProbes(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"valid empty cloud", `{"inventories":[]}`, "", 200},
		{"bad AK", `{"error":{"code":"SYS.1006","details":"access key id: test-access-key does not existed or disabled"}}`, "ZSTACK_ACCESS_KEY_INVALID", 403},
		{"bad SK", `{"error":{"code":"SYS.1006","details":"wrong accessKey signature"}}`, "ZSTACK_SIGNATURE_INVALID", 403},
		{"license failure", `{"error":{"code":"SYS.1006","details":"AccessKey authentication requires valid Enterprise license"}}`, "ZSTACK_LICENSE_REQUIRED", 403},
		{"other HTTP service", `<html>wrong service</html>`, "ZSTACK_REQUEST_FAILED", 200},
		{"other JSON service", `{}`, "ZSTACK_REQUEST_FAILED", 200},
		{"API unavailable", `{"error":{"code":"SYS.1000"}}`, "ZSTACK_REQUEST_FAILED", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/hosts" || !validTestAccessKey(r) {
					t.Error("validation did not use the signed hosts API")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			d := New()
			d.diagnostics = &endpointDiagnostics{timeout: time.Second,
				tcp: func(context.Context, string) error { t.Error("unexpected TCP probe"); return nil },
				ip:  func(context.Context, string) ipProbeResult { t.Error("unexpected ping"); return ipResponded },
			}
			err := d.ValidateCredentials(t.Context(), provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if got := validationErrorCode(t, err); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestCloudValidationPortRefusalAndResponseTimeout(t *testing.T) {
	t.Run("closed port", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		server.Close()
		err := New().ValidateCredentials(t.Context(), provider.Credentials{Endpoint: server.URL, PublicKey: "ak", PrivateKey: "sk"})
		if got := validationErrorCode(t, err); got != "ZSTACK_PORT_REFUSED" {
			t.Fatal(got)
		}
	})
	t.Run("API response timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer server.Close()
		d, err := NewWithClientOptions(apiclient.WithRequestTimeout(50 * time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		err = d.ValidateCredentials(t.Context(), provider.Credentials{Endpoint: server.URL, PublicKey: "ak", PrivateKey: "sk"})
		if got := validationErrorCode(t, err); got != "ZSTACK_RESPONSE_TIMEOUT" {
			t.Fatal(got)
		}
	})
}
