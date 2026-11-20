package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQuorumMultiContactAlternateSurvivesUnavailableOriginal(t *testing.T) {
	for _, name := range []string{"closed original", "held original"} {
		t.Run(name, func(t *testing.T) {
			held := make(chan struct{})
			var f *fixture
			var originalKey string
			f, originalKey = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				quorumDiscoveryClientRequest(t, r)
				close(held)
				<-r.Context().Done()
			}))
			client := f.client(t)
			if name == "closed original" {
				f.server.Close()
			}
			var alternate *httptest.Server
			var alternateKey string
			var discoveries, observations atomic.Int32
			nonce := make(chan string, 1)
			alternate, alternateKey = quorumDiscoveryPeerEndpoint(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if name == "held original" {
					select {
					case <-held:
					case <-r.Context().Done():
						return
					}
				}
				if r.URL.Path == ClusterQuorumDiscoveryPath {
					discoveries.Add(1)
					// Record the actual discovery nonce; the later leader read must be fresh.
					requestNonce := quorumDiscoveryClientRequest(t, r)
					nonce <- requestNonce
					hints := ClusterQuorumDiscovery{ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: 1, Nonce: requestNonce, Members: []ClusterMember{{ID: 11, Address: "127.0.0.1:9010", Role: "voter", KeyFingerprint: alternateKey, EnrollmentOrigin: alternate.URL}}}
					_ = json.NewEncoder(w).Encode(hints)
					return
				}
				observations.Add(1)
				requestNonce := quorumFactsClientRequest(t, r)
				if requestNonce == <-nonce {
					t.Error("facts reused contact discovery nonce")
				}
				_, _ = io.WriteString(w, quorumFactsClientResponse(f, alternateKey, requestNonce))
			}))
			allowed := []string{originalKey, alternateKey}
			sort.Strings(allowed)
			cert, key := catalogMember(t, f)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			facts, err := client.DiscoverClusterQuorumFactsFromContacts(ctx, cert, key, allowed, map[string]string{alternate.URL: alternateKey})
			if err != nil || facts.LeaderID != 11 || facts.Phase != "frozen" || facts.PhaseRevision != 37 || len(facts.Members) != 2 || facts.Members[0].KeyFingerprint != alternateKey || ctx.Err() != nil {
				t.Fatalf("alternate failed to supply exact fresh leader facts: %+v %v", facts, err)
			}
			if discoveries.Load() != 1 || observations.Load() != 1 {
				t.Fatalf("unexpected requests %d/%d", discoveries.Load(), observations.Load())
			}
		})
	}
}

func TestQuorumMultiContactRejectsInvalidContactsBeforeHTTP(t *testing.T) {
	for _, name := range []string{"unknown key", "noncanonical key", "HTTP", "path", "slash", "userinfo", "too many", "unsorted allowed", "empty allowed"} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			f, originalKey := quorumFactsClientEndpoint(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			cert, key := catalogMember(t, f)
			contacts := map[string]string{f.server.URL: originalKey}
			allowed := []string{originalKey}
			switch name {
			case "unknown key":
				contacts[f.server.URL] = strings.Repeat("0", 64)
			case "noncanonical key":
				contacts[f.server.URL] = strings.Repeat("A", 64)
			case "HTTP":
				contacts = map[string]string{"http://127.0.0.1:8800": originalKey}
			case "path":
				contacts = map[string]string{f.server.URL + "/private": originalKey}
			case "slash":
				contacts = map[string]string{f.server.URL + "/": originalKey}
			case "userinfo":
				contacts = map[string]string{"https://user@127.0.0.1:8800": originalKey}
			case "too many":
				for i := 0; i < 512; i++ {
					contacts[fmt.Sprintf("https://127.0.0.1:%d", 10000+i)] = originalKey
				}
			case "unsorted allowed":
				allowed = []string{strings.Repeat("f", 64), strings.Repeat("0", 64)}
			case "empty allowed":
				allowed = nil
			}
			got, err := f.client(t).DiscoverClusterQuorumFactsFromContacts(t.Context(), cert, key, allowed, contacts)
			assertQuorumFactsClientZero(t, got, err)
			if requests.Load() != 0 {
				t.Fatal("invalid contact reached authenticated HTTP handler")
			}
		})
	}
}

func TestQuorumMultiContactPinsAlternateBeforeCredentials(t *testing.T) {
	var requests atomic.Int32
	f, originalKey := quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	alternate, _ := quorumDiscoveryPeerEndpoint(t, f, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	// The expected key is allowed, but the alternate's actual key differs.
	expected := strings.Repeat("0", 64)
	allowed := []string{originalKey, expected}
	sort.Strings(allowed)
	cert, key := catalogMember(t, f)
	got, err := f.client(t).DiscoverClusterQuorumFactsFromContacts(t.Context(), cert, key, allowed, map[string]string{alternate.URL: expected})
	assertQuorumFactsClientZero(t, got, err)
	if requests.Load() != 0 {
		t.Fatal("wrong alternate TLS key received credential-bearing request")
	}
}

func TestQuorumMultiContactAllFailedReturnsZero(t *testing.T) {
	var calls atomic.Int32
	f, originalKey := quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		quorumDiscoveryClientRequest(t, r)
		w.WriteHeader(503)
	}))
	alternate, alternateKey := quorumDiscoveryPeerEndpoint(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		quorumDiscoveryClientRequest(t, r)
		w.WriteHeader(404)
	}))
	allowed := []string{originalKey, alternateKey}
	sort.Strings(allowed)
	cert, key := catalogMember(t, f)
	got, err := f.client(t).DiscoverClusterQuorumFactsFromContacts(t.Context(), cert, key, allowed, map[string]string{alternate.URL: alternateKey, f.server.URL: originalKey})
	assertQuorumFactsClientZero(t, got, err)
	if calls.Load() != 2 {
		t.Fatalf("primary duplicate or contact omitted: %d", calls.Load())
	}
}

func TestQuorumMultiContactCancellationJoinsLocalWorker(t *testing.T) {
	entered, observedCancel, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f, originalKey := quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		quorumDiscoveryClientRequest(t, r)
		w.Header().Set("Location", "https://127.0.0.1:8800/blocked")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	cert, key := catalogMember(t, f)
	client := f.client(t)
	// CheckRedirect runs inside this actual local Do call. Holding its return
	// proves that cancellation alone cannot finish the outer discovery helper.
	client.http.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		close(entered)
		<-r.Context().Done()
		close(observedCancel)
		<-release
		return r.Context().Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		facts ClusterQuorumFacts
		err   error
	}
	done := make(chan result, 1)
	go func() {
		facts, err := client.DiscoverClusterQuorumFactsFromContacts(ctx, cert, key, []string{originalKey}, nil)
		done <- result{facts, err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("actual local request did not reach redirect callback")
	}
	cancel()
	select {
	case <-observedCancel:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("local request did not observe cancellation")
	}
	select {
	case <-done:
		close(release)
		t.Fatal("outer discovery returned before actual local HTTP request returned")
	default:
	}
	close(release)
	select {
	case got := <-done:
		assertQuorumFactsClientZero(t, got.facts, got.err)
		if !errors.Is(got.err, context.Canceled) {
			t.Fatal("lost cancellation cause", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("outer discovery did not join actual local request return")
	}
}

func TestQuorumMultiContactCorrectedTupleIsNotSuppressed(t *testing.T) {
	var f *fixture
	var originalKey, alternateKey, providerKey string
	var alternate, provider *httptest.Server
	firstProvider := make(chan struct{})
	var factsCalls atomic.Int32
	writeHints := func(w http.ResponseWriter, r *http.Request, contactKey, address string) {
		nonce := quorumDiscoveryClientRequest(t, r)
		hints := ClusterQuorumDiscovery{ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: 1, Nonce: nonce, Members: []ClusterMember{
			{ID: 11, Address: address, Role: "voter", KeyFingerprint: providerKey, EnrollmentOrigin: provider.URL},
			{ID: 12, Address: "127.0.0.1:9011", Role: "spare", KeyFingerprint: contactKey},
		}}
		_ = json.NewEncoder(w).Encode(hints)
	}
	f, originalKey = quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeHints(w, r, originalKey, "127.0.0.1:9099") }))
	provider, providerKey = quorumDiscoveryPeerEndpoint(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := quorumFactsClientRequest(t, r)
		if factsCalls.Add(1) == 1 {
			close(firstProvider)
		}
		_, _ = io.WriteString(w, quorumFactsClientResponse(f, providerKey, nonce))
	}))
	alternate, alternateKey = quorumDiscoveryPeerEndpoint(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-firstProvider:
		case <-r.Context().Done():
			return
		}
		writeHints(w, r, alternateKey, "127.0.0.1:9010")
	}))
	allowed := []string{originalKey, alternateKey}
	sort.Strings(allowed)
	cert, key := catalogMember(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	facts, err := f.client(t).DiscoverClusterQuorumFactsFromContacts(ctx, cert, key, allowed, map[string]string{alternate.URL: alternateKey})
	if err != nil || facts.LeaderID != 11 || factsCalls.Load() != 2 {
		t.Fatalf("corrected candidate tuple was suppressed: %+v %v calls=%d", facts, err, factsCalls.Load())
	}
}

func TestQuorumMultiContactWinnerWaitsForLocalJoinAndRechecksParent(t *testing.T) {
	for _, name := range []string{"parent alive", "parent canceled during join", "parent expires during join"} {
		t.Run(name, func(t *testing.T) {
			entered, workerCanceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseWorker()
			f, originalKey := quorumFactsClientEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				quorumDiscoveryClientRequest(t, r)
				w.Header().Set("Location", "https://127.0.0.1:8800/blocked")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			var alternate *httptest.Server
			var alternateKey string
			var factsCalls atomic.Int32
			alternate, alternateKey = quorumDiscoveryPeerEndpoint(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-entered:
				case <-r.Context().Done():
					return
				}
				if r.URL.Path == ClusterQuorumDiscoveryPath {
					nonce := quorumDiscoveryClientRequest(t, r)
					hints := ClusterQuorumDiscovery{ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: 1, Nonce: nonce, Members: []ClusterMember{{ID: 11, Address: "127.0.0.1:9010", Role: "voter", KeyFingerprint: alternateKey, EnrollmentOrigin: alternate.URL}}}
					_ = json.NewEncoder(w).Encode(hints)
					return
				}
				nonce := quorumFactsClientRequest(t, r)
				factsCalls.Add(1)
				_, _ = io.WriteString(w, quorumFactsClientResponse(f, alternateKey, nonce))
			}))
			cert, key := catalogMember(t, f)
			client := f.client(t)
			// The valid winner cancels this actual local Do, but its redirect callback
			// retains ownership until explicitly released below.
			client.http.CheckRedirect = func(r *http.Request, via []*http.Request) error {
				close(entered)
				<-r.Context().Done()
				close(workerCanceled)
				<-release
				return r.Context().Err()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "parent expires during join" {
				var expiryCancel context.CancelFunc
				ctx, expiryCancel = context.WithTimeout(ctx, time.Second)
				defer expiryCancel()
			}
			allowed := []string{originalKey, alternateKey}
			sort.Strings(allowed)
			type result struct {
				facts ClusterQuorumFacts
				err   error
			}
			done := make(chan result, 1)
			go func() {
				facts, err := client.DiscoverClusterQuorumFactsFromContacts(ctx, cert, key, allowed, map[string]string{alternate.URL: alternateKey})
				done <- result{facts, err}
			}()
			select {
			case <-workerCanceled:
			case <-time.After(2 * time.Second):
				t.Fatal("valid winner did not initiate local worker cancellation")
			}
			if ctx.Err() != nil || factsCalls.Load() != 1 {
				t.Fatalf("winner not accepted before parent expired: parent=%v facts=%d", ctx.Err(), factsCalls.Load())
			}
			select {
			case <-done:
				t.Fatal("fresh winner escaped before actual held Do returned")
			default:
			}
			switch name {
			case "parent canceled during join":
				cancel()
			case "parent expires during join":
				<-ctx.Done()
			}
			// Parent cancellation still must not let facts or errors escape the held Do.
			select {
			case <-done:
				t.Fatal("winner join ended before actual held Do returned")
			default:
			}
			releaseWorker()
			select {
			case got := <-done:
				if name == "parent alive" {
					if got.err != nil || got.facts.LeaderID != 11 || got.facts.Members[0].KeyFingerprint != alternateKey || ctx.Err() != nil {
						t.Fatalf("joined winner was rejected or changed: %+v %v", got.facts, got.err)
					}
				} else {
					assertQuorumFactsClientZero(t, got.facts, got.err)
					want := context.Canceled
					if name == "parent expires during join" {
						want = context.DeadlineExceeded
					}
					if !errors.Is(got.err, want) {
						t.Fatalf("actual parent cause missing after winner join: %v", got.err)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("actual held Do returned without discovery helper joining")
			}
		})
	}
}
