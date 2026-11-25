//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// refusalMember is one fixture member with the proof worker, the journal and
// the gate a closed member has, proving through the real proof on the real
// database processes.
type refusalMember struct {
	app         *App
	eligibility *materialEligibility
	gate        *businessHandlerGate
	refusals    *businessRefusals
	journal     *bytes.Buffer
}

func (f *candidateGrantFixture) proveMember(index int) func(context.Context) (bool, CandidateProofMetrics, error) {
	return func(ctx context.Context) (bool, CandidateProofMetrics, error) {
		recorder := &candidateProofRecorder{}
		ctx = context.WithValue(ctx, candidateProofMetricsKey{}, recorder)
		ready, err := checkJoinedStoreMaterialReadiness(ctx, f.stores[index], f.bundles[index], f.databases[index].ID, Config{DataDir: filepath.Dir(f.receivers[index].root)})
		return ready, recorder.snapshot(), err
	}
}

func (f *candidateGrantFixture) refusalMember(t *testing.T, index int, now func() time.Time, prove func(context.Context) (bool, CandidateProofMetrics, error)) *refusalMember {
	t.Helper()
	m := &refusalMember{journal: &bytes.Buffer{}, refusals: newBusinessRefusals()}
	log := slog.New(slog.NewTextHandler(m.journal, nil))
	m.app = &App{log: log}
	if prove == nil {
		prove = f.proveMember(index)
	}
	m.eligibility = newJournalledMaterialEligibility(m.app, log, now, prove)
	m.eligibility.setOnInvalidated(m.app.closeBusinessAdmission)
	m.app.materialEligibility = m.eligibility
	m.gate = newBusinessHandlerGate()
	m.gate.refusals = m.refusals
	m.gate.requireMaterialEligibility()
	m.gate.setMaterialEligibility(m.eligibility)
	m.gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(m.eligibility.Stop)
	return m
}

// read is one read through the member's gate: the status and the refusal
// code on it.
func (m *refusalMember) read() (int, string) {
	rec := httptest.NewRecorder()
	m.gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
	return rec.Code, rec.Header().Get(businessRefusalHeader)
}

func (m *refusalMember) mustProve(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	if ready, err := m.eligibility.Reprove(ctx); !ready || err != nil {
		t.Fatalf("%s: ready=%t err=%v", what, ready, err)
	}
	if code, _ := m.read(); code != http.StatusNoContent || !m.eligibility.Eligible() {
		t.Fatalf("%s: the member does not serve reads (%d)", what, code)
	}
}

// A member whose admission is revoked while its last proof is still valid
// closes at the proof that gets the database's refusal, not when the proof
// it holds runs out. Three real voters; the refusal is the real grant's.
func TestNativeRevokedMemberClosesAtItsNextProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	member := f.refusalMember(t, 1, time.Now, nil)
	member.mustProve(t, ctx, "initial proof")

	if _, err := f.stores[0].RevokeClusterAdmission(ctx, f.expected().ClusterID, f.keys[1]); err != nil {
		t.Fatal(err)
	}
	revokedAt := time.Now()
	// Until its next proof the member serves on the confirmation it holds.
	if code, _ := member.read(); code != http.StatusNoContent {
		t.Fatalf("the member refused reads before any proof learned of the revocation: %d", code)
	}
	ready, err := member.eligibility.Reprove(ctx)
	closedAfter := time.Since(revokedAt)
	if ready || !store.RefusesMember(err, f.keys[1]) || !errors.Is(err, store.ErrClusterAdmissionRejected) {
		t.Fatalf("proof of a revoked member: ready=%t err=%v", ready, err)
	}
	if member.eligibility.Eligible() {
		t.Fatalf("the revoked member still admits requests %v after its proof was refused", closedAfter)
	}
	if code, reason := member.read(); code != http.StatusServiceUnavailable || reason != refusalProofMissing {
		t.Fatalf("a read on the revoked member: %d, refusal %q", code, reason)
	}
	if closedAfter >= materialEligibilityWindow {
		t.Fatalf("closed %v after the revocation, not sooner than the proof's window of %v", closedAfter, materialEligibilityWindow)
	}
	journal := member.journal.String()
	if count(journal, journalClosed) != 1 || !strings.Contains(journal, "caller=proof_failed first_of_period=true") {
		t.Fatalf("the close logged:\n%s", journal)
	}
	if count(journal, journalFailed) != 1 || !strings.Contains(journal, "failed_proofs=1 err_class=admission_rejected") {
		t.Fatalf("the refused proof logged:\n%s", journal)
	}
	t.Logf("closed %v after the revocation; the proof's window is %v", closedAfter.Round(time.Millisecond), materialEligibilityWindow)

	// The other members were not refused: their grant names the revoked
	// member's key no longer, and they keep the proof they hold.
	other := f.refusalMember(t, 0, time.Now, nil)
	if _, _, err := f.proveMember(0)(ctx); store.RefusesMember(err, f.keys[0]) {
		t.Fatalf("another member read the revocation as its own: %v", err)
	}
	_ = other
}

// One grant covers this member's key and the key of every member that
// publishes material. When another member is revoked between the proof's
// two grants, the database refuses that member's key: the refusal names it,
// and the proving member keeps the confirmation it holds.
func TestNativeAnotherMembersRevocationMidProofKeepsTheMemberOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	expected := f.expected()
	member := f.refusalMember(t, 0, time.Now, nil)
	member.mustProve(t, ctx, "initial proof")

	var once sync.Once
	revoked := make(chan error, 1)
	f.mu.Lock()
	f.before = func() {
		once.Do(func() {
			_, err := f.stores[0].RevokeClusterAdmission(ctx, expected.ClusterID, f.keys[2])
			revoked <- err
		})
	}
	f.mu.Unlock()
	ready, err := member.eligibility.Reprove(ctx)
	select {
	case revokeErr := <-revoked:
		if revokeErr != nil {
			t.Fatal(revokeErr)
		}
	default:
		t.Fatal("the proof did not reach its peer requests, so nothing was revoked under it")
	}
	if ready {
		t.Fatal("a proof whose closing grant was refused reported ready")
	}
	if store.RefusesMember(err, f.keys[0]) || store.Causes(err, store.CauseMemberRefused) != 0 {
		t.Fatalf("the proof returned another member's refusal as this member's: %v", err)
	}
	if !member.eligibility.Eligible() {
		t.Fatal("removing another member closed this member")
	}
	if code, reason := member.read(); code != http.StatusNoContent {
		t.Fatalf("a read after another member's revocation: %d, refusal %q", code, reason)
	}
	if journal := member.journal.String(); count(journal, journalClosed) != 0 {
		t.Fatalf("a close was logged:\n%s", journal)
	}
	// What the closing grant returned: a refusal that names the revoked
	// member's key and nobody else's.
	_, _, grantErr := f.stores[0].GrantClusterMembershipKeys(ctx, expected, []string{f.keys[0], f.keys[2]})
	var refusal *store.ClusterAdmissionRefusal
	if !errors.As(grantErr, &refusal) || refusal.Key != f.keys[2] || !errors.Is(grantErr, store.ErrClusterAdmissionRejected) {
		t.Fatalf("grant naming a revoked member's key: %v", grantErr)
	}
	if store.RefusesMember(grantErr, f.keys[0]) || !store.RefusesMember(grantErr, f.keys[2]) {
		t.Fatalf("the refusal does not say whose it is: %v", grantErr)
	}
}

// A maintenance phase that does not serve refuses every grant. That is the
// cluster's state, not a verdict on the member: it keeps the confirmation it
// holds, and proves again once the cluster serves.
func TestNativeMaintenancePhaseDoesNotCloseAMemberAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	member := f.refusalMember(t, 1, time.Now, nil)
	member.mustProve(t, ctx, "initial proof")

	leader := f.leaderStore(t, ctx)
	serving, err := leader.ReadMaintenancePhase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := leader.TransitionMaintenancePhase(ctx, serving, store.MaintenanceFreezing)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := member.eligibility.Reprove(ctx)
	if ready || store.RefusesMember(err, f.keys[1]) {
		t.Fatalf("proof during a maintenance phase: ready=%t err=%v", ready, err)
	}
	if !member.eligibility.Eligible() {
		t.Fatal("a maintenance phase closed the member at once")
	}
	if journal := member.journal.String(); count(journal, journalClosed) != 0 {
		t.Fatalf("a close was logged:\n%s", journal)
	}
	if _, err := leader.TransitionMaintenancePhase(ctx, frozen, store.MaintenanceServing); err != nil {
		t.Fatal(err)
	}
	member.mustProve(t, ctx, "proof after the cluster serves again")
}

func (f *candidateGrantFixture) leaderStore(t *testing.T, ctx context.Context) *store.Store {
	t.Helper()
	leader, err := f.databases[0].Leader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, database := range f.databases {
		if database.ID == leader.ID {
			return f.stores[i]
		}
	}
	t.Fatalf("leader %d is not a fixture member", leader.ID)
	return nil
}

// The database's refusal is its answer also when the proof's own time has
// run out by the time it is handled: the member closes and is not carried
// for another period as if nothing had answered. The refusal is the real
// grant's; that the window has passed is forced by the test's clock and an
// ended proof context.
func TestNativeRefusalAfterTheProofWindowStillCloses(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	clock := &materialTestClock{now: time.Now()}
	var late atomic.Bool
	prove := func(proofCtx context.Context) (bool, CandidateProofMetrics, error) {
		if !late.Load() {
			return f.proveMember(1)(proofCtx)
		}
		// The proof runs to the grant, which refuses; before the failure
		// is handled the window is over and the proof's context has ended.
		_, _, grantErr := f.stores[1].GrantClusterMembershipKeys(proofCtx, f.expected(), []string{f.keys[1]})
		clock.Advance(materialEligibilityWindow + time.Second)
		ended, end := context.WithCancel(proofCtx)
		end()
		ready, err := materialProofFailure(ended, f.keys[1], grantErr)
		return ready, CandidateProofMetrics{}, err
	}
	member := f.refusalMember(t, 1, clock.Now, prove)
	member.mustProve(t, ctx, "initial proof")
	if _, err := f.stores[0].RevokeClusterAdmission(ctx, f.expected().ClusterID, f.keys[1]); err != nil {
		t.Fatal(err)
	}
	late.Store(true)
	ready, err := member.eligibility.Reprove(ctx)
	if ready || !store.RefusesMember(err, f.keys[1]) {
		t.Fatalf("a refusal handled after the window: ready=%t err=%v", ready, err)
	}
	if member.eligibility.Eligible() {
		t.Fatal("a refusal that arrived after the window was taken for no answer: the member is still open")
	}
	if journal := member.journal.String(); count(journal, journalClosed) != 1 || !strings.Contains(journal, "err_class=admission_rejected") {
		t.Fatalf("the late refusal logged:\n%s", journal)
	}
	// The same moment without a refusal is no answer and keeps the member.
	if ready, err := materialProofFailure(func() context.Context {
		ended, end := context.WithCancel(ctx)
		end()
		return ended
	}(), f.keys[1], context.DeadlineExceeded); ready || !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended proof without a refusal: ready=%t err=%v", ready, err)
	}
}
