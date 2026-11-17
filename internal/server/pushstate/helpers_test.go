package pushstate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

var errInjected = errors.New("injected failure")

// faultyStore is the real store with failures injected on demand: the
// pushed state tables, their migration and their transactions are the real
// ones.
type faultyStore struct {
	*store.Store
	failWrites atomic.Int32
	failLoad   atomic.Bool
	writes     atomic.Int32
	mu         sync.Mutex
	batches    []*pstate.Batch
}

func (f *faultyStore) WritePushedState(ctx context.Context, b *pstate.Batch) (pstate.Result, error) {
	f.writes.Add(1)
	f.mu.Lock()
	f.batches = append(f.batches, b)
	f.mu.Unlock()
	if f.failWrites.Load() > 0 {
		f.failWrites.Add(-1)
		return pstate.Result{}, errInjected
	}
	return f.Store.WritePushedState(ctx, b)
}

func (f *faultyStore) LoadPushedState(ctx context.Context) (pstate.Snapshot, error) {
	if f.failLoad.Load() {
		return pstate.Snapshot{}, errInjected
	}
	return f.Store.LoadPushedState(ctx)
}

func openStore(t testing.TB, dsn string) *faultyStore {
	t.Helper()
	if dsn == "" {
		dsn = "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	}
	s, err := store.Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &faultyStore{Store: s}
}

// durableDSN names an in-memory database that outlives the stores opened on
// it, as a database file does: a store kept open for the test holds it, so a
// test can close its store ("kill the core") and open another on the same
// rows without replaying the whole migration chain on a file each time.
func durableDSN(t testing.TB) string {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "_durable?mode=memory&cache=shared"
	openStore(t, dsn)
	return dsn
}

// fakeClock is a settable wall clock and monotonic reading.
type fakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func newFakeClock(at string) *fakeClock {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		panic(err)
	}
	return &fakeClock{wall: t}
}

func (c *fakeClock) clock() Clock {
	return Clock{
		Wall: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.wall },
		Mono: func() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.mono },
	}
}

// advance moves both readings forward, as time passing does.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.wall, c.mono = c.wall.Add(d), c.mono+d
	c.mu.Unlock()
}

// setWall moves only the wall clock, as an operator changing it does.
func (c *fakeClock) setWall(at string) {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	c.wall = t
	c.mu.Unlock()
}

// The packages of the tests: acme's authority and the package whose keys it
// answers for, a third party, and the project's own authority.
var (
	acmeGrants  = extapi.CertGrants{IDPrefixes: []string{"acme_"}, FeatureNS: []string{"acme"}, AuthorityID: "acme_quota", Tables: []string{extapi.KindFleetCapacity}, MinIsolation: extapi.IsolationLandlock}
	rogueGrants = extapi.CertGrants{IDPrefixes: []string{"rogue_"}, FeatureNS: []string{"rogue"}, AuthorityID: "rogue_quota", Tables: []string{extapi.KindFleetCapacity}, MinIsolation: extapi.IsolationLandlock}
	projGrants  = extapi.CertGrants{IDPrefixes: []string{"quota"}, FeatureNS: []string{"", "gpuworks"}, AuthorityID: "quota", Tables: []string{extapi.KindFleetCapacity}, MinIsolation: extapi.IsolationLandlock}

	acmeQuota = Package{ID: "acme_quota", Publisher: "acme", Grants: acmeGrants, Verified: true}
	acmeBackup  = Package{ID: "acme_backup", Publisher: "acme", Grants: acmeGrants, Verified: true, Features: []extapi.FeatureDecl{
		{Key: "acme.backup", Presentation: extapi.PresentationPromote, Gate: extapi.GateAuthority},
		{Key: "acme.reports", Presentation: extapi.PresentationPromote, Gate: extapi.GateAuthority},
		{Key: "acme.tools", Presentation: extapi.PresentationConceal, Gate: extapi.GateSelf},
		{Key: "acme.free", Presentation: extapi.PresentationPromote, Gate: extapi.GateNone},
	}}
	acmeOps      = Package{ID: "acme_ops", Publisher: "acme", Grants: acmeGrants, Verified: true}
	rogueQuota = Package{ID: "rogue_quota", Publisher: "rogue", Grants: rogueGrants, Verified: true}
	projQuota  = Package{ID: "quota", Publisher: extapi.ProjectPublisher, Grants: projGrants, Verified: true, Features: []extapi.FeatureDecl{
		{Key: "audit", Presentation: extapi.PresentationPromote, Gate: extapi.GateAuthority},
	}}
	allPackages = []Package{acmeQuota, acmeBackup, acmeOps, rogueQuota, projQuota}

	acmeCodes = []string{"ACME_CARD_LIMIT", "ACME_EXPIRED", "ACME_GRACE", "ACME_MAINTENANCE", "ACME_NODE_LIMIT", "ACME_OVER_CAPACITY", "ACME_QUARANTINE"}
)

func pusherOf(p Package, perms ...string) Pusher {
	return Pusher{ID: p.ID, Publisher: p.Publisher, Grants: p.Grants, Permissions: perms, Codes: acmeCodes, Generation: 1}
}

// harness is a State over a real store, with a fake clock and recorded
// effects.
type harness struct {
	t        testing.TB
	st       *faultyStore
	clk      *fakeClock
	s        *State
	fleet    atomic.Pointer[[]fleetcap.Node]
	reads    atomic.Int32
	reapply  atomic.Int32
	features atomic.Int32
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	dropped  []Override
}

func newHarness(t testing.TB, st *faultyStore, clk *fakeClock, mutate ...func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, st: st, clk: clk}
	empty := []fleetcap.Node{}
	h.fleet.Store(&empty)
	cfg := Config{
		Store: st, Packages: allPackages, CoreVersion: "v26.12.0", Clock: clk.clock(),
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		TickInterval: time.Millisecond, SelectFloor: time.Millisecond,
		Fleet: func(context.Context) ([]fleetcap.Node, error) {
			h.reads.Add(1)
			return *h.fleet.Load(), nil
		},
		Effects: Effects{
			ReapplyHolds:    func() { h.reapply.Add(1) },
			FeaturesChanged: func() { h.features.Add(1) },
			OverrideSuperseded: func(o Override) {
				h.mu.Lock()
				h.dropped = append(h.dropped, o)
				h.mu.Unlock()
			},
		},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	h.s = New(cfg)
	return h
}

func (h *harness) load() *harness {
	h.t.Helper()
	if err := h.s.Load(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	return h
}

// run starts the writer tick; stop (also at cleanup) ends it.
func (h *harness) run() *harness {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() {
		defer close(h.done)
		_ = h.s.Run(ctx)
	}()
	h.t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.cancel = nil
	}
}

func (h *harness) setFleet(nodes ...fleetcap.Node) { h.fleet.Store(&nodes) }

// push sends kind's body as pusher at gen and fails the test on a refusal.
func (h *harness) push(p Pusher, kind string, gen uint64, body string) *extapi.StateAck {
	h.t.Helper()
	ack, w := h.try(p, kind, gen, body)
	if w != nil {
		h.t.Fatalf("push %s gen %d refused: %s %v %s", kind, gen, w.Code, w.Params, w.Message)
	}
	return ack
}

func (h *harness) try(p Pusher, kind string, gen uint64, body string) (*extapi.StateAck, *extapi.WireError) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.s.Push(ctx, p, &extapi.StatePush{Kind: kind, Schema: 1, Generation: gen, Body: json.RawMessage(body)})
}

// recombine evaluates timelines at the fake clock's now, as the step timer
// does when it fires.
func (h *harness) recombine() {
	h.s.mu.Lock()
	holds, features := h.s.recombineLocked()
	h.s.mu.Unlock()
	h.s.effects(holds, features)
}

// selectNow runs one capacity selection pass, as the selection runner does.
func (h *harness) selectNow() {
	h.t.Helper()
	h.s.fleetDirty.Store(true)
	if _, _, err := h.s.selectPass(); err != nil {
		h.t.Fatal(err)
	}
}

// waitFor polls cond until it holds (the tick and selection run on their
// own goroutines).
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func i64(v int64) *int64 { return &v }
