package enrollment

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Discovery work remains ephemeral. A contact yields dial hints; only a
// separately pinned provider read can yield a fresh leader observation.
type quorumDiscoveryWork struct {
	origin, contactKey string
	provider           *ClusterMember
}

type quorumDiscoveryWorkResult struct {
	hints ClusterQuorumDiscovery
	facts ClusterQuorumFacts
	err   error
}

// DiscoverClusterQuorumFactsFromContacts includes the configured endpoint and
// independently known public origins. Two workers share contact and provider
// work, so discovery never nests another set of concurrent HTTP requests.
func (c *Client) DiscoverClusterQuorumFactsFromContacts(parent context.Context, certificate, key []byte, allowedContactKeys []string, contacts map[string]string) (ClusterQuorumFacts, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	reject := func(err error) (ClusterQuorumFacts, error) {
		return ClusterQuorumFacts{}, errors.Join(err, ctx.Err())
	}
	if len(allowedContactKeys) == 0 || len(allowedContactKeys) > 512 {
		return reject(errors.New("quorum discovery requires bounded allowed contact keys"))
	}
	allowed := append([]string(nil), allowedContactKeys...)
	for i, fingerprint := range allowed {
		if !validCatalogDigest(fingerprint) || i > 0 && allowed[i-1] >= fingerprint {
			return reject(errors.New("allowed contact keys must be canonical, unique and sorted"))
		}
	}
	u, err := url.Parse(c.endpoint)
	if err != nil || u.Path != certificatePath {
		return reject(errors.New("invalid configured discovery endpoint"))
	}
	primary := u.Scheme + "://" + u.Host
	if !canonicalEnrollmentOrigin(primary) {
		return reject(errors.New("configured discovery origin is not canonical HTTPS"))
	}
	if len(contacts) > 512 {
		return reject(errors.New("too many initial discovery contacts"))
	}
	origins := make([]string, 0, len(contacts))
	for origin, fingerprint := range contacts {
		i := sort.SearchStrings(allowed, fingerprint)
		if !canonicalEnrollmentOrigin(origin) || !validCatalogDigest(fingerprint) || i == len(allowed) || allowed[i] != fingerprint {
			return reject(errors.New("discovery contact must bind a canonical HTTPS origin to an allowed key"))
		}
		if origin != primary {
			origins = append(origins, origin)
		}
	}
	if len(origins) >= 512 {
		return reject(errors.New("too many initial discovery contacts"))
	}
	sort.Strings(origins)
	pendingContacts := []quorumDiscoveryWork{{origin: primary, contactKey: contacts[primary]}}
	for _, origin := range origins {
		pendingContacts = append(pendingContacts, quorumDiscoveryWork{origin: origin, contactKey: contacts[origin]})
	}
	if err := ctx.Err(); err != nil {
		return reject(err)
	}
	workersCtx, stopWorkers := context.WithCancel(ctx)
	jobs := make(chan quorumDiscoveryWork)
	results := make(chan quorumDiscoveryWorkResult, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for work := range jobs {
				if workersCtx.Err() != nil {
					return
				}
				results <- c.runQuorumDiscoveryWork(workersCtx, certificate, key, allowed, primary, work)
			}
		}()
	}
	joined := false
	joinWorkers := func() {
		if joined {
			return
		}
		stopWorkers()
		close(jobs)
		// Wait for actual local HTTP calls and their cleanup, including cancellation.
		workers.Wait()
		joined = true
	}
	defer joinWorkers()
	pendingProviders := []quorumDiscoveryWork{}
	providers := make(map[string]bool)
	failures := []error{errors.New("no discovery voter provided bound quorum facts")}
	active := 0
	for active != 0 || len(pendingContacts) != 0 || len(pendingProviders) != 0 {
		var dispatch chan quorumDiscoveryWork
		var next quorumDiscoveryWork
		if active < 2 {
			if len(pendingProviders) != 0 {
				dispatch, next = jobs, pendingProviders[0]
			} else if len(pendingContacts) != 0 {
				dispatch, next = jobs, pendingContacts[0]
			}
		}
		select {
		case <-ctx.Done():
			return reject(ctx.Err())
		case dispatch <- next:
			active++
			if next.provider != nil {
				pendingProviders = pendingProviders[1:]
			} else {
				pendingContacts = pendingContacts[1:]
			}
		case result := <-results:
			active--
			if result.err != nil {
				failures = append(failures, result.err)
				continue
			}
			if result.facts.LeaderID != 0 {
				// Join before checking the parent's actual state and returning facts.
				joinWorkers()
				if err := ctx.Err(); err != nil {
					return reject(err)
				}
				return result.facts, nil
			}
			for _, candidate := range result.hints.Members {
				if candidate.Role != "voter" || candidate.EnrollmentOrigin == "" {
					continue
				}
				identity := candidate.EnrollmentOrigin + "\x00" + candidate.KeyFingerprint + "\x00" + strconv.FormatUint(candidate.ID, 10) + "\x00" + candidate.Address
				if providers[identity] {
					continue
				}
				if len(providers) == 512 {
					return reject(errors.New("too many discovered quorum providers"))
				}
				providers[identity] = true
				pendingProviders = append(pendingProviders, quorumDiscoveryWork{provider: &candidate})
			}
		}
	}
	return reject(errors.Join(failures...))
}

func (c *Client) runQuorumDiscoveryWork(ctx context.Context, certificate, key []byte, allowed []string, primary string, work quorumDiscoveryWork) quorumDiscoveryWorkResult {
	if work.provider != nil {
		facts, err := c.fetchDiscoveryCandidate(ctx, certificate, key, *work.provider)
		return quorumDiscoveryWorkResult{facts: facts, err: err}
	}
	contact := c
	if work.origin != primary {
		var err error
		contact, err = NewClient(work.origin+certificatePath, c.clusterID, c.pin, proto.ClusterProtocolVersion)
		if err != nil {
			return quorumDiscoveryWorkResult{err: err}
		}
		defer contact.Close()
	}
	contactKeys := allowed
	if work.contactKey != "" {
		contactKeys = []string{work.contactKey}
	}
	hints, _, err := contact.FetchClusterQuorumDiscovery(ctx, certificate, key, contactKeys)
	return quorumDiscoveryWorkResult{hints: hints, err: err}
}
