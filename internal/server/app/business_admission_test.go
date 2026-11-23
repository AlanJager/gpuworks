//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// businessAdmissionPaths are requests of every kind the gate admits: writes,
// inference, an agent's upgraded connection, reads and a terminal.
var businessAdmissionPaths = []struct {
	method, path string
	upgrade      bool
}{
	{http.MethodPost, "/api/v1/nodes", false},
	{http.MethodPost, "/v1/chat/completions", false},
	{http.MethodGet, "/api/v1/agent/connect", true},
	{http.MethodGet, "/v1/models", false},
	{http.MethodGet, "/api/v1/nodes/node-1/terminal", false},
	{http.MethodPost, "/api/v1/playground/v1/chat/completions", false},
	{http.MethodGet, "/api/v1/nodes", false},
	{http.MethodHead, "/api/v1/nodes", false},
	{http.MethodOptions, "/api/v1/nodes", false},
}

// stalledConfirmation is a member whose first confirmation succeeded and
// whose renewal then waits on storage: the second proof blocks until the test
// releases it with the answer the database finally gives.
type stalledConfirmation struct {
	clock    atomic.Int64
	proofs   atomic.Int32
	stalled  chan struct{}
	answer   chan error
	served   atomic.Int32
	gate     *businessHandlerGate
	renewal  chan struct{}
	eligible *materialEligibility
}

func newStalledConfirmation(t *testing.T) *stalledConfirmation {
	t.Helper()
	c := &stalledConfirmation{stalled: make(chan struct{}), answer: make(chan error, 1), renewal: make(chan struct{})}
	c.clock.Store(time.Now().UTC().UnixNano())
	c.eligible = newMaterialEligibility(func() time.Time { return time.Unix(0, c.clock.Load()) }, func(ctx context.Context) (bool, error) {
		if c.proofs.Add(1) == 1 {
			return true, nil
		}
		close(c.stalled)
		select {
		case err := <-c.answer:
			return err == nil, err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}, time.Hour)
	c.gate = newBusinessHandlerGate()
	c.gate.requireMaterialEligibility()
	c.gate.setMaterialEligibility(c.eligible)
	c.gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c.served.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	if ready, err := c.eligible.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("first confirmation: ready=%t err=%v", ready, err)
	}
	return c
}

// stallRenewal starts the renewal and returns once it waits on storage.
func (c *stalledConfirmation) stallRenewal(t *testing.T) {
	t.Helper()
	go func() {
		defer close(c.renewal)
		_, _ = c.eligible.Reprove(t.Context())
	}()
	select {
	case <-c.stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("renewal did not start")
	}
	t.Cleanup(func() {
		select {
		case c.answer <- context.Canceled:
		default:
		}
		<-c.renewal
	})
}

// requestAll sends one request of every kind and fails when any answer is
// not want or took long enough to have waited for the stalled renewal.
func (c *stalledConfirmation) requestAll(t *testing.T, want int) {
	t.Helper()
	for _, tc := range businessAdmissionPaths {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.upgrade {
			r.Header.Set("Upgrade", "websocket")
		}
		w := httptest.NewRecorder()
		started := time.Now()
		c.gate.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, w.Code, want)
		}
		// A request that waited for a confirmation would wait at least one
		// grant timeout. The bound sits just below that, so a loaded runner
		// does not fail it.
		if waited := time.Since(started); waited > store.ClusterMembershipGrantTimeout-time.Second {
			t.Fatalf("%s %s waited %v for a confirmation", tc.method, tc.path, waited)
		}
	}
}

func TestBusinessRequestsAreAdmittedWhileConfirmationWaitsOnStorage(t *testing.T) {
	c := newStalledConfirmation(t)
	c.requestAll(t, http.StatusNoContent)
	// The renewal starts eight seconds into the first confirmation's window
	// and then waits on storage for as long as the slowest stall measured.
	c.clock.Add((8 * time.Second).Nanoseconds())
	c.stallRenewal(t)
	c.requestAll(t, http.StatusNoContent)
	// The first confirmation runs out while its renewal is still waiting:
	// no answer is not a refusal.
	c.clock.Add((8 * time.Second).Nanoseconds())
	if c.eligible.EligibleFor(time.Nanosecond) {
		t.Fatal("the first confirmation is still inside its window")
	}
	c.requestAll(t, http.StatusNoContent)
	if got := c.proofs.Load(); got != 2 {
		t.Fatalf("requests started %d confirmations of their own", got-2)
	}
	if got, want := c.served.Load(), int32(3*len(businessAdmissionPaths)); got != want {
		t.Fatalf("business served %d requests, want %d", got, want)
	}
	// Storage answers: the renewal succeeds and admission simply continues.
	c.answer <- nil
	<-c.renewal
	c.requestAll(t, http.StatusNoContent)
}

// The earlier confirmation is still inside its window when the database
// answers that this member is not admitted. The answer closes the member at
// once; it does not wait for the window to end.
func TestBusinessRequestsAreRefusedAtOnceWhenDenialArrivesInsideTheWindow(t *testing.T) {
	c := newStalledConfirmation(t)
	c.stallRenewal(t)
	c.clock.Add((4 * time.Second).Nanoseconds())
	c.requestAll(t, http.StatusNoContent)
	c.answer <- store.ErrClusterAdmissionRejected
	<-c.renewal
	served := c.served.Load()
	c.requestAll(t, http.StatusServiceUnavailable)
	if c.served.Load() != served {
		t.Fatal("a refused request reached the business handler")
	}
}

func TestBusinessRequestsAreRefusedOnceConfirmationIsDenied(t *testing.T) {
	c := newStalledConfirmation(t)
	c.stallRenewal(t)
	c.clock.Add(materialEligibilityWindow.Nanoseconds())
	c.requestAll(t, http.StatusNoContent)
	// The database answers that this member is not admitted. With no earlier
	// confirmation left, that closes the member for every kind of request.
	c.answer <- store.ErrClusterAdmissionRejected
	<-c.renewal
	served := c.served.Load()
	c.requestAll(t, http.StatusServiceUnavailable)
	if c.served.Load() != served {
		t.Fatal("business served a request after the refusal")
	}
	w := httptest.NewRecorder()
	c.gate.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("liveness after refusal = %d", w.Code)
	}
}

// The periodic authorization refresh is the other source of a verdict: a
// refusal closes the member at once, a refresh that got no answer does not.
func TestAuthorizationRefreshVerdictClosesBusinessRequests(t *testing.T) {
	c := newStalledConfirmation(t)
	a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: c.eligible, businessHTTP: c.gate}}
	undecided := a.closedTransportAuthorizationRefresh(func(context.Context) error { return context.DeadlineExceeded })
	if err := undecided(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("undecided refresh = %v", err)
	}
	c.requestAll(t, http.StatusNoContent)
	refused := a.closedTransportAuthorizationRefresh(func(context.Context) error { return store.ErrClusterAdmissionRejected })
	if err := refused(t.Context()); !errors.Is(err, store.ErrClusterAdmissionRejected) {
		t.Fatalf("refused refresh = %v", err)
	}
	c.requestAll(t, http.StatusServiceUnavailable)
}

// A request that was admitted finishes even when admission ends meanwhile;
// only requests that arrive afterwards are refused.
func TestBusinessRequestAdmittedBeforeAdmissionEndsFinishes(t *testing.T) {
	for _, change := range []string{"expired", "invalidated", "withdrawn"} {
		t.Run(change, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UTC().UnixNano())
			eligibility := newMaterialEligibility(func() time.Time { return time.Unix(0, clock.Load()) },
				func(context.Context) (bool, error) { return true, nil }, time.Hour)
			if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
				t.Fatalf("confirmation: ready=%t err=%v", ready, err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			gate := newBusinessHandlerGate()
			gate.requireMaterialEligibility()
			gate.setMaterialEligibility(eligibility)
			var calls atomic.Int32
			gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			first := make(chan int, 1)
			go func() {
				w := httptest.NewRecorder()
				gate.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", nil))
				first <- w.Code
			}()
			<-entered
			switch change {
			case "expired":
				clock.Add(materialEligibilityWindow.Nanoseconds())
			case "invalidated":
				eligibility.Invalidate()
			case "withdrawn":
				gate.setBusinessHandler(nil)
			}
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("request after admission ended = %d", w.Code)
			}
			close(release)
			if status := <-first; status != http.StatusAccepted {
				t.Fatalf("admitted request = %d", status)
			}
			if calls.Load() != 1 {
				t.Fatal("business handler ran after admission ended")
			}
		})
	}
}

func TestMemberRouteKeyAnswersFromConfirmedMembershipWithoutGrant(t *testing.T) {
	members := []store.ClusterTransportMember{{ID: 1, KeyFingerprint: "local"}, {ID: 2, KeyFingerprint: "peer"}}
	pick := func(key string) func([]store.ClusterTransportMember) string {
		return func(members []store.ClusterTransportMember) string {
			for _, member := range members {
				if member.KeyFingerprint == key {
					return key
				}
			}
			return ""
		}
	}
	confirmed := func(ok bool) func() ([]store.ClusterTransportMember, bool) {
		return func() ([]store.ClusterTransportMember, bool) {
			if !ok {
				return nil, false
			}
			return members, true
		}
	}
	// A grant that never returns stands for storage that does not answer.
	stalled := func(ctx context.Context) ([]store.ClusterTransportMember, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if key, err := memberRouteKey(ctx, confirmed(true), stalled, pick("peer")); err != nil || key != "peer" {
		t.Fatalf("confirmed member: key=%q err=%v", key, err)
	}
	if waited := time.Since(started); waited > 100*time.Millisecond {
		t.Fatalf("confirmed member waited %v for a grant", waited)
	}
	// Without a confirmed list young enough, the grant decides, and its
	// failure is the answer.
	if _, err := memberRouteKey(ctx, confirmed(false), stalled, pick("peer")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no confirmed list: err=%v", err)
	}
	var grants atomic.Int32
	fresh := func(context.Context) ([]store.ClusterTransportMember, error) {
		grants.Add(1)
		return append([]store.ClusterTransportMember{{ID: 3, KeyFingerprint: "joined"}}, members...), nil
	}
	// A member admitted after the last confirmation is found by a grant.
	if key, err := memberRouteKey(t.Context(), confirmed(true), fresh, pick("joined")); err != nil || key != "joined" || grants.Load() != 1 {
		t.Fatalf("new member: key=%q err=%v grants=%d", key, err, grants.Load())
	}
	// A key no list names is refused after that one grant.
	if key, err := memberRouteKey(t.Context(), confirmed(true), fresh, pick("stranger")); err != nil || key != "" || grants.Load() != 2 {
		t.Fatalf("unknown member: key=%q err=%v grants=%d", key, err, grants.Load())
	}
	denied := func(context.Context) ([]store.ClusterTransportMember, error) {
		return nil, store.ErrClusterAdmissionRejected
	}
	if _, err := memberRouteKey(t.Context(), confirmed(false), denied, pick("peer")); !errors.Is(err, store.ErrClusterAdmissionRejected) {
		t.Fatalf("refused grant: err=%v", err)
	}
}

// Renewals that only run out of time carry a member through a stall, but not
// forever: once the last successful confirmation is older than the limit the
// member refuses requests, and the next success admits them again.
func TestBusinessRequestsAreRefusedWhenNoConfirmationSucceedsWithinTheLimit(t *testing.T) {
	var clock atomic.Int64
	base := time.Now().UTC()
	var answer atomic.Pointer[error]
	eligibility := newMaterialEligibility(func() time.Time { return base.Add(time.Duration(clock.Load())) }, func(context.Context) (bool, error) {
		if err := answer.Load(); err != nil {
			// A grant that used up its own timeout without an answer.
			clock.Add(int64(store.ClusterMembershipGrantTimeout))
			return false, *err
		}
		return true, nil
	}, time.Hour)
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	gate.setMaterialEligibility(eligibility)
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := func() int {
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		return w.Code
	}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("first confirmation: ready=%t err=%v", ready, err)
	}
	timeout := error(context.DeadlineExceeded)
	answer.Store(&timeout)
	closedAt := time.Duration(-1)
	for attempt := 0; attempt < 20 && closedAt < 0; attempt++ {
		clock.Add(int64(materialReproofInterval))
		_, _ = eligibility.Reprove(t.Context())
		if code := request(); code == http.StatusServiceUnavailable {
			closedAt = time.Duration(clock.Load())
		} else if code != http.StatusNoContent {
			t.Fatalf("request during unanswered renewals = %d", code)
		}
	}
	if closedAt < materialUnconfirmedLimit || closedAt > materialUnconfirmedLimit+materialEligibilityWindow {
		t.Fatalf("member closed %v after its last successful confirmation, want just past %v", closedAt, materialUnconfirmedLimit)
	}
	// Further unanswered renewals do not reopen it.
	clock.Add(int64(materialReproofInterval))
	_, _ = eligibility.Reprove(t.Context())
	if code := request(); code != http.StatusServiceUnavailable {
		t.Fatalf("request after the limit = %d", code)
	}
	answer.Store(nil)
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("confirmation after the stall: ready=%t err=%v", ready, err)
	}
	if code := request(); code != http.StatusNoContent {
		t.Fatalf("request after a successful confirmation = %d", code)
	}
}

// A member that never confirmed is not admitted on a renewal without an answer.
func TestBusinessRequestsNeedOneSuccessfulConfirmation(t *testing.T) {
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return false, context.DeadlineExceeded }, time.Hour)
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	gate.setMaterialEligibility(eligibility)
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	_, _ = eligibility.Reprove(t.Context())
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("request on a member that never confirmed = %d", w.Code)
	}
}
