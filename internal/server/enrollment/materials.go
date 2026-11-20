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
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// FetchSecrets proves possession of the persisted member key on a fresh TLS
// connection to the pinned enrollment origin. It does not persist the result,
// retry unknown outcomes, release the join guard, or imply asset readiness.
func (c *Client) FetchSecrets(ctx context.Context, certificatePEM, keyPEM []byte) (BootstrapSecrets, error) {
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil || len(pair.Certificate) != 1 {
		return BootstrapSecrets{}, errors.New("material transfer requires one member certificate and its private key")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return BootstrapSecrets{}, errors.New("invalid material recipient certificate")
	}
	transport := c.http.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = []tls.Certificate{pair}
	defer transport.CloseIdleConnections()
	client := *c.http
	client.Transport = transport
	endpoint := strings.TrimSuffix(c.endpoint, certificatePath) + SecretsPath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return BootstrapSecrets{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return BootstrapSecrets{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return BootstrapSecrets{}, fmt.Errorf("material request rejected: HTTP %d", response.StatusCode)
	}
	const maxResponse = 1 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return BootstrapSecrets{}, errors.New("material response is incomplete or too large")
	}
	var result BootstrapSecrets
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return BootstrapSecrets{}, errors.New("invalid material response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return BootstrapSecrets{}, errors.New("trailing material response")
	}
	if result.ClusterID != c.clusterID || result.Protocol != proto.ClusterProtocolVersion || len(result.MasterSecret) == 0 || len(result.MasterSecret) > 64<<10 {
		return BootstrapSecrets{}, errors.New("material response identity or secret rejected")
	}
	now := time.Now().UTC()
	if _, err := identity.LoadAuthority([]byte(result.CACertificatePEM), []byte(result.CAKeyPEM), c.clusterID, c.pin, now); err != nil {
		return BootstrapSecrets{}, errors.New("material authority rejected")
	}
	trust, err := identity.NewTrust([]byte(result.CACertificatePEM), c.clusterID, c.pin, now)
	if err != nil {
		return BootstrapSecrets{}, errors.New("material trust rejected")
	}
	if _, err := trust.VerifyMember(leaf, now); err != nil {
		return BootstrapSecrets{}, errors.New("material recipient rejected")
	}
	return result, nil
}
