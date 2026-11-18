package pushstate

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// appliedLog records Effects.Applied.
type appliedLog struct {
	mu  sync.Mutex
	all []string
}

func (l *appliedLog) add(owner, kind string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, fmt.Sprintf("%s/%s/%d", owner, kind, n))
}

func (l *appliedLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.all)
}

// Applied reports each committed push with its entry count, and never a
// push whose commit failed or that a newer push superseded.
func TestAppliedReportsCommittedPushesOnly(t *testing.T) {
	st := openStore(t, "")
	log := &appliedLog{}
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z"), func(c *Config) { c.Effects.Applied = log.add }).load()
	ops := pusherOf(acmeOps, "push:node_holds")

	// Two pushes queued before the tick runs: the first is superseded.
	first := make(chan *extapi.StateAck, 1)
	go func() {
		a, _ := h.try(ops, extapi.KindNodeHolds, 1, `{"nodes":{"7":{"code":"ACME_MAINTENANCE"}}}`)
		first <- a
	}()
	h.waitFor("the first push to queue", func() bool { h.s.mu.Lock(); defer h.s.mu.Unlock(); return len(h.s.pending) == 1 })
	second := make(chan *extapi.StateAck, 1)
	go func() {
		a, _ := h.try(ops, extapi.KindNodeHolds, 2, `{"nodes":{"7":{"code":"ACME_MAINTENANCE"},"9":{"code":"ACME_MAINTENANCE"}}}`)
		second <- a
	}()
	h.waitFor("the second push to replace it", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		p := h.s.pending[ownerKind{"acme_ops", extapi.KindNodeHolds}]
		return p != nil && p.part.gen == 2
	})
	h.run()
	if a := <-first; a == nil || a.SupersededBy != 2 {
		t.Fatalf("first push = %+v, want superseded", a)
	}
	if a := <-second; a == nil || a.AppliedAt == nil {
		t.Fatalf("second push = %+v, want applied", a)
	}

	st.failWrites.Store(1)
	if _, w := h.try(pusherOf(acmeAuthority), extapi.KindAvailability, 1, backupFull); w == nil {
		t.Fatal("a push whose commit failed was acknowledged")
	}
	h.push(pusherOf(acmeAuthority), extapi.KindAvailability, 1, backupFull)

	want := []string{"acme_ops/node_holds/2", "acme_authority/availability/1"}
	if got := log.got(); !slices.Equal(got, want) {
		t.Fatalf("applied = %v, want %v", got, want)
	}
}

func TestEntriesOf(t *testing.T) {
	for _, c := range []struct {
		table extapi.Table
		want  int
	}{
		{&extapi.AvailabilityV1{Rows: make([]extapi.AvailabilityRow, 3)}, 3},
		{&extapi.NodeHoldsV1{Nodes: map[string]extapi.Notice{"1": {}, "2": {}}}, 2},
		{&extapi.BannersV1{}, 0},
		{&extapi.AttestationsV1{Attestations: make([]extapi.Attestation, 4)}, 4},
		{&extapi.FleetCapacityV1{}, 1},
	} {
		if got := EntriesOf(c.table); got != c.want {
			t.Errorf("EntriesOf(%s) = %d, want %d", c.table.TableKind(), got, c.want)
		}
	}
}
