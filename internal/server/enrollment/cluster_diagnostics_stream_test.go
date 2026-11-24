package enrollment

import (
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A part takes longer than any request this client otherwise makes: its
// whole-request timeout must not apply to the stream, only the caller's
// context. The member here answers its headers at once and its part after
// the client's own timeout has passed.
func TestADiagnosticsPartIsNotCutByTheClientsRequestTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ClusterDiagnosticsPath || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(ClusterDiagnosticsClockHeader, time.Now().UTC().Format(time.RFC3339Nano))
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		time.Sleep(3 * timeout)
		_, _ = w.Write([]byte("the part"))
	}))
	server.StartTLS()
	defer server.Close()

	// Any key pair serves as the caller's certificate against this server.
	leaf := server.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(leaf.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]})
	private := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})

	transport := server.Client().Transport.(*http.Transport).Clone()
	client := &Client{endpoint: server.URL + certificatePath, http: &http.Client{Transport: transport, Timeout: timeout}}
	answer, err := client.OpenClusterDiagnostics(t.Context(), certificate, private, time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	if answer.Clock.IsZero() || answer.Sent.IsZero() || answer.MemberKey == "" || answer.RoundTrip <= 0 || answer.RoundTrip > timeout {
		t.Fatalf("answer = %+v", answer)
	}
	raw, err := io.ReadAll(answer.Body)
	if err != nil || string(raw) != "the part" {
		t.Fatalf("the part was cut at the client's request timeout: %q, %v", raw, err)
	}
}
