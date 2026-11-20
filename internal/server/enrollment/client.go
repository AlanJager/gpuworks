// Package enrollment implements authenticated bootstrap requests. It does not
// start the application, change Raft membership or persist private material.
package enrollment

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

const certificatePath = "/api/v1/cluster/enrollment/certificate"
const maxCertificateResponse = 128 << 10

type Client struct {
	endpoint, clusterID, pin string
	http                     *http.Client
}

// NewClient pins the destination before any invitation is sent. EnrollmentURL
// comes from the administrator's invitation, along with its expected identity.
// Redirects and environment proxies cannot relocate the credential exchange.
func NewClient(enrollmentURL, clusterID, pin string, protocol uint) (*Client, error) {
	u, err := url.Parse(enrollmentURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Path != certificatePath {
		return nil, errors.New("enrollment requires the exact HTTPS certificate endpoint without credentials or query")
	}
	if protocol != proto.ClusterProtocolVersion {
		return nil, errors.New("unsupported enrollment protocol")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, errors.New("invalid enrollment endpoint port")
		}
	}
	tlsConfig, err := identity.EnrollmentTLSConfig(clusterID, pin, u.Hostname())
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 5 * time.Second,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 16 << 10,
		DisableKeepAlives: true,
	}
	return &Client{endpoint: u.String(), clusterID: clusterID, pin: pin,
		http: &http.Client{Transport: transport, Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

// Exchange uses a key already persisted by the caller. A failed request has an
// unknown commit outcome; retain the key and use Recover instead of assuming
// the invitation is unconsumed or generating another member identity.
func (c *Client) Exchange(ctx context.Context, token string, keyPEM []byte, address string) (dto.ClusterCertificateResponse, error) {
	return c.ExchangeWithOrigin(ctx, token, keyPEM, address, "")
}

func (c *Client) ExchangeWithOrigin(ctx context.Context, token string, keyPEM []byte, address, origin string) (dto.ClusterCertificateResponse, error) {
	if token == "" {
		return dto.ClusterCertificateResponse{}, errors.New("invitation is required")
	}
	return c.request(ctx, token, keyPEM, address, origin, false)
}

// Recover requests only a public certificate for the existing CSR binding.
// It never retrieves a CA private key, master secret or other private assets.
func (c *Client) Recover(ctx context.Context, keyPEM []byte, address string) (dto.ClusterCertificateResponse, error) {
	return c.RecoverForOrigin(ctx, keyPEM, address, "")
}

func (c *Client) RecoverForOrigin(ctx context.Context, keyPEM []byte, address, origin string) (dto.ClusterCertificateResponse, error) {
	return c.request(ctx, "", keyPEM, address, origin, true)
}

func (c *Client) request(ctx context.Context, token string, keyPEM []byte, address, origin string, recover bool) (dto.ClusterCertificateResponse, error) {
	var result dto.ClusterCertificateResponse
	if origin != "" && !canonicalEnrollmentOrigin(origin) {
		return result, errors.New("member public origin must be canonical HTTPS")
	}
	address, err := identity.NormalizeMemberAddress(address)
	if err != nil {
		return result, err
	}
	host, _, _ := net.SplitHostPort(address)
	csr, err := identity.NewMemberCSR(keyPEM)
	if err != nil {
		return result, err
	}
	expectedKey, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		return result, err
	}
	endpoint := c.endpoint
	var body any = dto.ClusterCertificateRequest{ClusterID: c.clusterID, Protocol: proto.ClusterProtocolVersion, Address: address, EnrollmentOrigin: origin, CSRPEM: string(csr)}
	wantStatus := http.StatusCreated
	if recover {
		endpoint += "/recover"
		wantStatus = http.StatusOK
		body = dto.ClusterCertificateRecoveryRequest{ClusterID: c.clusterID, Protocol: proto.ClusterProtocolVersion, CSRPEM: string(csr)}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return result, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		// Do not echo a remote body: it may reflect the invitation or include
		// untrusted operational instructions. Keep the status for diagnosis.
		return result, fmt.Errorf("enrollment request rejected: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxCertificateResponse+1))
	if err != nil || len(data) > maxCertificateResponse {
		return result, errors.New("enrollment response is incomplete or too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return dto.ClusterCertificateResponse{}, errors.New("invalid enrollment response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return dto.ClusterCertificateResponse{}, errors.New("trailing enrollment response")
	}
	if result.ClusterID != c.clusterID || result.Protocol != proto.ClusterProtocolVersion || result.Address != address || result.KeyFingerprint != expectedKey {
		return dto.ClusterCertificateResponse{}, errors.New("enrollment response identity mismatch")
	}
	trust, err := identity.NewTrust([]byte(result.CACertificatePEM), c.clusterID, c.pin, time.Now().UTC())
	if err != nil {
		return dto.ClusterCertificateResponse{}, err
	}
	pair, err := tls.X509KeyPair([]byte(result.CertificatePEM), keyPEM)
	if err != nil {
		return dto.ClusterCertificateResponse{}, errors.New("enrollment certificate does not match the persisted member key")
	}
	if len(pair.Certificate) != 1 {
		return dto.ClusterCertificateResponse{}, errors.New("enrollment response must contain exactly one member certificate")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return dto.ClusterCertificateResponse{}, err
	}
	fingerprint, err := trust.VerifyMember(leaf, time.Now().UTC())
	if err != nil || fingerprint != expectedKey {
		return dto.ClusterCertificateResponse{}, errors.New("enrollment certificate identity rejected")
	}
	if err := leaf.VerifyHostname(host); err != nil {
		return dto.ClusterCertificateResponse{}, errors.New("enrollment certificate member address mismatch")
	}
	if origin != "" {
		publicURL, _ := url.Parse(origin)
		if err := leaf.VerifyHostname(publicURL.Hostname()); err != nil {
			return dto.ClusterCertificateResponse{}, errors.New("enrollment certificate public origin mismatch")
		}
	}
	return result, nil
}
