package enrollment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// FetchClusterQuorumDiscovery contacts only an independently allowed TLS owner.
// Returned origins are hints; they do not establish current quorum authority.
func (c *Client) FetchClusterQuorumDiscovery(ctx context.Context, certificate, key []byte, allowedContactKeys []string) (ClusterQuorumDiscovery, string, error) {
	allowed := append([]string(nil), allowedContactKeys...)
	if len(allowed) == 0 || len(allowed) > 512 {
		return ClusterQuorumDiscovery{}, "", errors.New("quorum discovery requires bounded allowed contact keys")
	}
	for i, fingerprint := range allowed {
		if !validCatalogDigest(fingerprint) || i > 0 && allowed[i-1] >= fingerprint {
			return ClusterQuorumDiscovery{}, "", errors.New("allowed contact keys must be canonical, unique and sorted")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	reject := func(err error) (ClusterQuorumDiscovery, string, error) {
		return ClusterQuorumDiscovery{}, "", errors.Join(err, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return reject(err)
	}
	var nonceBytes [32]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return reject(err)
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	body, err := json.Marshal(ClusterQuorumFactsRequest{Nonce: nonce})
	if err != nil {
		return reject(err)
	}
	transport := c.http.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	verify := transport.TLSClientConfig.VerifyConnection
	if verify == nil {
		return reject(errors.New("contact key binding requires pinned endpoint verification"))
	}
	transport.TLSClientConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if err := verify(state); err != nil {
			return err
		}
		if len(state.PeerCertificates) == 0 {
			return errors.New("discovery contact omitted its certificate")
		}
		sum := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
		fingerprint := hex.EncodeToString(sum[:])
		i := sort.SearchStrings(allowed, fingerprint)
		if i == len(allowed) || allowed[i] != fingerprint {
			return errors.New("discovery contact key is not allowed")
		}
		return nil
	}
	defer transport.CloseIdleConnections()
	httpClient := *c.http
	httpClient.Transport = transport
	contact := *c
	contact.http = &httpClient
	data, peerKey, err := contact.memberRequestForPeerWithBodyAndKey(ctx, certificate, key, ClusterQuorumDiscoveryPath, body, 512<<10, "", http.StatusOK)
	if err != nil {
		return reject(err)
	}
	result, err := DecodeClusterQuorumDiscovery(data)
	if err != nil {
		return reject(err)
	}
	if result.ClusterID != c.clusterID || result.CAFingerprint != c.pin || result.Protocol != proto.ClusterProtocolVersion || result.Nonce != nonce {
		return reject(errors.New("quorum discovery identity or nonce rejected"))
	}
	for _, member := range result.Members {
		if member.KeyFingerprint == peerKey {
			if err := ctx.Err(); err != nil {
				return reject(err)
			}
			return result, peerKey, nil
		}
	}
	return reject(errors.New("discovery contact is absent from returned hints"))
}

// DiscoverClusterQuorumFacts follows explicit voter HTTPS hints to a fresh,
// separately authenticated leader observation. Neither step authorizes startup.
func (c *Client) DiscoverClusterQuorumFacts(ctx context.Context, certificate, key []byte, allowedContactKeys []string) (ClusterQuorumFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	reject := func(err error) (ClusterQuorumFacts, error) {
		return ClusterQuorumFacts{}, errors.Join(err, ctx.Err())
	}
	hints, _, err := c.FetchClusterQuorumDiscovery(ctx, certificate, key, allowedContactKeys)
	if err != nil {
		return reject(err)
	}
	jobs := make(chan ClusterMember, len(hints.Members))
	for _, candidate := range hints.Members {
		if candidate.Role == "voter" && candidate.EnrollmentOrigin != "" {
			jobs <- candidate
		}
	}
	close(jobs)
	workersCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	winner := make(chan ClusterQuorumFacts, 1)
	failures := make(chan error, len(hints.Members))
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for candidate := range jobs {
				if workersCtx.Err() != nil {
					return
				}
				facts, err := c.fetchDiscoveryCandidate(workersCtx, certificate, key, candidate)
				if err != nil {
					failures <- err
					continue
				}
				select {
				case winner <- facts:
					stopWorkers()
				default:
				}
				return
			}
		}()
	}
	// Cancellation alone is not completion: every HTTP worker must return before
	// the observation or error escapes this call.
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return reject(err)
	}
	select {
	case facts := <-winner:
		return facts, nil
	default:
	}
	close(failures)
	errorsSeen := []error{errors.New("no discovery voter provided bound quorum facts")}
	for err := range failures {
		errorsSeen = append(errorsSeen, err)
	}
	return reject(errors.Join(errorsSeen...))
}

func (c *Client) fetchDiscoveryCandidate(ctx context.Context, certificate, key []byte, candidate ClusterMember) (ClusterQuorumFacts, error) {
	provider, err := NewClient(candidate.EnrollmentOrigin+certificatePath, c.clusterID, c.pin, proto.ClusterProtocolVersion)
	if err != nil {
		return ClusterQuorumFacts{}, err
	}
	defer provider.Close()
	facts, err := provider.FetchClusterQuorumFacts(ctx, certificate, key, candidate.KeyFingerprint)
	if err != nil {
		return ClusterQuorumFacts{}, err
	}
	for _, member := range facts.Members {
		if member.ID == candidate.ID && member.Address == candidate.Address && member.KeyFingerprint == candidate.KeyFingerprint && member.Role == "voter" && facts.LeaderID == candidate.ID {
			if err := ctx.Err(); err != nil {
				return ClusterQuorumFacts{}, err
			}
			return facts, nil
		}
	}
	return ClusterQuorumFacts{}, errors.New("quorum facts leader differs from discovery candidate")
}
