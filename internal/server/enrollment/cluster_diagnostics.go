package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// ClusterDiagnosticsPath is the member-authenticated route on which a member
// gives its own part of a diagnostics collection: its log in the window, its
// database role records and its stall reports, read from its directories
// alone. It is served by the member's own enrollment listener, which answers
// while the member has no leader and while its business is closed, the
// states in which the other members' records are needed most.
const ClusterDiagnosticsPath = "/_cluster/diagnostics"

// ClusterDiagnosticsClockHeader carries the answering member's clock (RFC
// 3339 with nanoseconds) at the moment it began to answer, sent before the
// part is read, so the asker can place that clock against its own.
const ClusterDiagnosticsClockHeader = "X-Gpuworks-Member-Clock"

// ClusterDiagnosticsRequest names the window of the part.
type ClusterDiagnosticsRequest struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// ClusterDiagnosticsAnswer is a member's part as it arrives: a gzipped tar.
type ClusterDiagnosticsAnswer struct {
	Body io.ReadCloser
	// Clock is the member's clock when it began to answer (zero when it
	// did not say), Sent this side's clock when the request was sent and
	// RoundTrip how long the answer's start then took.
	Clock     time.Time
	Sent      time.Time
	RoundTrip time.Duration
	// MemberKey is the fingerprint of the member key whose certificate
	// answered (the SHA-256 of its public key, as admissions name it).
	MemberKey string
}

type diagnosticsBody struct {
	io.ReadCloser
	transport *http.Transport
}

func (b diagnosticsBody) Close() error {
	err := b.ReadCloser.Close()
	b.transport.CloseIdleConnections()
	return err
}

// OpenClusterDiagnostics asks the member behind this client for its part of
// the window, authenticated with the caller's member certificate. The part
// is streamed: the caller reads it within its own bounds and closes it. A
// member that does not accept the caller's key answers with a StatusError.
func (c *Client) OpenClusterDiagnostics(ctx context.Context, certificate, key []byte, from, to time.Time) (ClusterDiagnosticsAnswer, error) {
	none := ClusterDiagnosticsAnswer{}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil || len(pair.Certificate) != 1 {
		return none, errors.New("member request requires one member certificate and its private key")
	}
	body, err := json.Marshal(ClusterDiagnosticsRequest{From: from, To: to})
	if err != nil {
		return none, err
	}
	transport := c.http.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = []tls.Certificate{pair}
	// The part is already compressed, and its length is not known up front.
	transport.DisableCompression = true
	client := *c.http
	client.Transport = transport
	// The whole answer is bounded by the caller's context, not by the
	// client's own request timeout.
	client.Timeout = 0
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.endpoint, certificatePath)+ClusterDiagnosticsPath, bytes.NewReader(body))
	if err != nil {
		transport.CloseIdleConnections()
		return none, err
	}
	request.Header.Set("Content-Type", "application/json")
	asked := time.Now()
	response, err := client.Do(request)
	if err != nil {
		transport.CloseIdleConnections()
		return none, err
	}
	answer := ClusterDiagnosticsAnswer{Sent: asked, RoundTrip: time.Since(asked)}
	if response.TLS != nil && len(response.TLS.PeerCertificates) > 0 {
		sum := sha256.Sum256(response.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo)
		answer.MemberKey = hex.EncodeToString(sum[:])
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		transport.CloseIdleConnections()
		return none, &StatusError{Code: response.StatusCode}
	}
	if clock, err := time.Parse(time.RFC3339Nano, response.Header.Get(ClusterDiagnosticsClockHeader)); err == nil {
		answer.Clock = clock
	}
	answer.Body = diagnosticsBody{ReadCloser: response.Body, transport: transport}
	return answer, nil
}
