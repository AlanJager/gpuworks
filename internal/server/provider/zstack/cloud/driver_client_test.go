package cloud

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
)

func TestCloudDriverRejectsInvalidClientOptions(t *testing.T) {
	for _, option := range []apiclient.ClientOption{nil, apiclient.WithReadRetries(-1), apiclient.WithOperationTimeout(0)} {
		if _, err := NewWithClientOptions(option); err == nil {
			t.Fatal("driver accepted invalid client options")
		}
	}
}

func TestCloudDriverKeepsClientPolicyWhenBoundToApplication(t *testing.T) {
	st, svc, _ := newCloudRuntimeFixture(t)
	options := []apiclient.ClientOption{apiclient.WithReadRetries(1), apiclient.WithRetryBackoff(time.Millisecond, time.Millisecond)}
	driver, err := NewWithClientOptions(options...)
	if err != nil {
		t.Fatal(err)
	}
	options[0] = apiclient.WithReadRetries(0)
	bound := driver.NewDriver(st, svc, nil).(*Driver)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validTestAccessKey(r) {
			t.Error("configured driver lost AccessKey authentication")
		}
		if requests.Add(1) == 1 {
			http.Error(w, "gateway unavailable", http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"inventories":[]}`)
	}))
	defer server.Close()
	if err := bound.ValidateCredentials(t.Context(), provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("driver's configured query retry policy was lost: requests=%d", requests.Load())
	}
}
