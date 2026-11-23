package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/pkg/catalogasset"
)

const CatalogPackagesPath = "/_cluster/catalog-packages"

type CatalogPackageIndex struct {
	ClusterID string   `json:"cluster_id"`
	Protocol  uint     `json:"protocol"`
	Digests   []string `json:"digests"`
}

// FetchCatalogIndex reads one bounded reference snapshot. It does not imply
// that packages have been copied or that this member is ready to serve.
func (c *Client) FetchCatalogIndex(ctx context.Context, certificatePEM, keyPEM []byte) (CatalogPackageIndex, error) {
	data, err := c.memberRequest(ctx, certificatePEM, keyPEM, CatalogPackagesPath, 1<<20)
	if err != nil {
		return CatalogPackageIndex{}, err
	}
	var result CatalogPackageIndex
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return CatalogPackageIndex{}, errors.New("invalid catalog index")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return CatalogPackageIndex{}, errors.New("trailing catalog index")
	}
	if result.ClusterID != c.clusterID || result.Protocol != proto.ClusterProtocolVersion || result.Digests == nil {
		return CatalogPackageIndex{}, errors.New("catalog index identity or digests rejected")
	}
	for i, digest := range result.Digests {
		if !validCatalogDigest(digest) || (i > 0 && result.Digests[i-1] >= digest) {
			return CatalogPackageIndex{}, errors.New("catalog index digests must be unique and sorted")
		}
	}
	if err := ctx.Err(); err != nil {
		return CatalogPackageIndex{}, err
	}
	return result, nil
}

// FetchCatalogPackage accepts only the exact canonical archive requested by
// digest. Transport errors never expose a partially decoded package.
func (c *Client) FetchCatalogPackage(ctx context.Context, certificatePEM, keyPEM []byte, digest string) (inferencecatalog.Archive, error) {
	if !validCatalogDigest(digest) {
		return inferencecatalog.Archive{}, errors.New("invalid catalog package digest")
	}
	data, err := c.memberRequest(ctx, certificatePEM, keyPEM, CatalogPackagesPath+"/"+digest, catalogasset.MaxPackageBytes)
	if err != nil {
		return inferencecatalog.Archive{}, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return inferencecatalog.Archive{}, errors.New("catalog package content digest mismatch")
	}
	archive, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(data))
	if err != nil {
		return inferencecatalog.Archive{}, errors.New("invalid catalog package archive")
	}
	canonical, err := inferencecatalog.PackageDigest(archive.FS)
	if err != nil || canonical != digest {
		return inferencecatalog.Archive{}, errors.New("catalog package is not the canonical archive")
	}
	if err := ctx.Err(); err != nil {
		return inferencecatalog.Archive{}, err
	}
	return archive, nil
}

func validCatalogDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, c := range digest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// StatusError is a member request the endpoint answered with another status
// than the one the request is answered with when it is served.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("member request rejected: HTTP %d", e.Code) }

// Unavailable reports the endpoint saying it cannot serve the request right
// now (503): the same request may be served a moment later.
func (e *StatusError) Unavailable() bool { return e.Code == http.StatusServiceUnavailable }

func (c *Client) memberRequest(ctx context.Context, certificatePEM, keyPEM []byte, path string, limit int64) ([]byte, error) {
	return c.memberRequestForPeer(ctx, certificatePEM, keyPEM, path, limit, "")
}

func (c *Client) memberRequestForPeer(ctx context.Context, certificatePEM, keyPEM []byte, path string, limit int64, expectedPeer string) ([]byte, error) {
	return c.memberRequestForPeerWithBody(ctx, certificatePEM, keyPEM, path, nil, limit, expectedPeer, http.StatusOK)
}

func (c *Client) memberRequestForPeerWithBody(ctx context.Context, certificatePEM, keyPEM []byte, path string, body []byte, limit int64, expectedPeer string, expectedStatus int) ([]byte, error) {
	data, _, err := c.memberRequestForPeerWithBodyAndKey(ctx, certificatePEM, keyPEM, path, body, limit, expectedPeer, expectedStatus)
	return data, err
}

func (c *Client) memberRequestForPeerWithBodyAndKey(ctx context.Context, certificatePEM, keyPEM []byte, path string, body []byte, limit int64, expectedPeer string, expectedStatus int) ([]byte, string, error) {
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil || len(pair.Certificate) != 1 {
		return nil, "", errors.New("member request requires one member certificate and its private key")
	}
	transport := c.http.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = []tls.Certificate{pair}
	if expectedPeer != "" {
		if !validCatalogDigest(expectedPeer) {
			return nil, "", errors.New("invalid expected member key")
		}
		verify := transport.TLSClientConfig.VerifyConnection
		if verify == nil {
			return nil, "", errors.New("member key binding requires pinned endpoint verification")
		}
		transport.TLSClientConfig.VerifyConnection = func(state tls.ConnectionState) error {
			if err := verify(state); err != nil {
				return err
			}
			if len(state.PeerCertificates) == 0 {
				return errors.New("member endpoint omitted its certificate")
			}
			key := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if hex.EncodeToString(key[:]) != expectedPeer {
				return errors.New("member endpoint key differs from expected transport owner")
			}
			return nil
		}
	}
	defer transport.CloseIdleConnections()
	client := *c.http
	client.Transport = transport
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.endpoint, certificatePath)+path, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		return nil, "", &StatusError{Code: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, "", errors.New("member response is incomplete or too large")
	}
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 {
		return data, "", nil
	}
	key := sha256.Sum256(response.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo)
	return data, hex.EncodeToString(key[:]), nil
}
