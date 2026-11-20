package engine

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

type fakeLeaser struct {
	held  []string
	fresh bool
	err   error
	asked []string
}

func (f *fakeLeaser) Lease(_ context.Context, _ string, want []string, _ bool, progress func(string)) ([]string, bool, error) {
	f.asked = want
	if progress != nil {
		progress("waiting for Slurm job 20: Resources")
	}
	return f.held, f.fresh, f.err
}

type statusSink struct {
	mu  sync.Mutex
	got []proto.InstanceStatus
}

func (s *statusSink) report(st proto.InstanceStatus) {
	s.mu.Lock()
	s.got = append(s.got, st)
	s.mu.Unlock()
}

func leaseEngine(t *testing.T, leaser CardLeaser, canRebind bool, gpus ...proto.GPUInfo) (*Engine, *statusSink) {
	t.Helper()
	sink := &statusSink{}
	e := NewWithProber(&headlessTestDriver{}, sink.report, slog.Default(), t.TempDir(), &gpu.FixedProber{VendorName: "nvidia", GPUs: gpus})
	e.SetCardLeaser(leaser, func() bool { return canRebind })
	return e, sink
}

func t4(index int, bdf string) proto.GPUInfo {
	c := nvidiaCard(index, bdf)
	c.Name, c.MemoryTotalBytes = "Tesla T4", 15<<30
	return c
}

func leaseSpec() proto.InstanceDesiredState {
	return proto.InstanceDesiredState{UUID: "inst-1", DesiredState: "running", GPUIndexes: []int{0}, GPUIdents: []string{"pci:0000:00:07.0"}}
}

func TestLeaseCardsRebindsToTheCardsTheAllocatorGave(t *testing.T) {
	leaser := &fakeLeaser{held: []string{"pci:0000:00:08.0"}, fresh: true}
	e, sink := leaseEngine(t, leaser, true, t4(0, "0000:00:07.0"), t4(1, "0000:00:08.0"))
	spec := leaseSpec()
	e.desired[spec.UUID] = spec
	rebound, err := e.leaseCards(context.Background(), &spec)
	if err != nil || !rebound {
		t.Fatalf("leaseCards = %v, %v; want rebound", rebound, err)
	}
	if !slices.Equal(spec.GPUIndexes, []int{1}) || !slices.Equal(spec.GPUIdents, []string{"pci:0000:00:08.0"}) {
		t.Fatalf("spec = %v %v; want device 1", spec.GPUIndexes, spec.GPUIdents)
	}
	if got := e.InstanceCards()["inst-1"]; !slices.Equal(got, []string{"pci:0000:00:08.0"}) {
		t.Fatalf("InstanceCards = %v; the desired entry was not rebound", got)
	}
	var rebind *proto.InstanceStatus
	for i := range sink.got {
		if len(sink.got[i].GPUIdents) > 0 {
			rebind = &sink.got[i]
		}
	}
	if rebind == nil || !slices.Equal(rebind.GPUIdents, []string{"pci:0000:00:08.0"}) {
		t.Fatalf("reports = %+v; want one naming the cards held", sink.got)
	}
	if !strings.Contains(sink.got[0].Message, "waiting for Slurm job 20") {
		t.Fatalf("first report = %+v; want the allocator's progress", sink.got[0])
	}
}

func TestLeaseCardsFailsWhenTheServerCannotRebind(t *testing.T) {
	leaser := &fakeLeaser{held: []string{"pci:0000:00:08.0"}, fresh: true}
	e, _ := leaseEngine(t, leaser, false, t4(0, "0000:00:07.0"), t4(1, "0000:00:08.0"))
	spec := leaseSpec()
	_, err := e.leaseCards(context.Background(), &spec)
	if err == nil || !strings.Contains(err.Error(), "cannot rebind") {
		t.Fatalf("err = %v; want the server's missing rebinding named", err)
	}
	if f := failureOf(err, "", ""); f.Code != proto.ErrorCardLeaseRebindUnsupported || f.Resource != "pci:0000:00:08.0" {
		t.Fatalf("failure = %+v; want CARD_LEASE_REBIND_UNSUPPORTED naming the cards given", f)
	}
}

func TestLeaseCardsRefusesADifferentKindOfCard(t *testing.T) {
	small := nvidiaCard(1, "0000:00:08.0")
	small.Name, small.MemoryTotalBytes = "Tesla P4", 8<<30
	leaser := &fakeLeaser{held: []string{"pci:0000:00:08.0"}, fresh: true}
	e, _ := leaseEngine(t, leaser, true, t4(0, "0000:00:07.0"), small)
	spec := leaseSpec()
	_, err := e.leaseCards(context.Background(), &spec)
	if err == nil || !strings.Contains(err.Error(), "Tesla P4") {
		t.Fatalf("err = %v; want the smaller card refused", err)
	}
	if f := failureOf(err, "", ""); f.Code != proto.ErrorCardLeaseCardMismatch {
		t.Fatalf("failure = %+v; want CARD_LEASE_CARD_MISMATCH", f)
	}
}

func TestLeaseCardsKeepsTheSpecWhenTheCardsMatch(t *testing.T) {
	leaser := &fakeLeaser{held: []string{"pci:0000:00:07.0"}}
	e, sink := leaseEngine(t, leaser, true, t4(0, "0000:00:07.0"))
	spec := leaseSpec()
	rebound, err := e.leaseCards(context.Background(), &spec)
	if err != nil || rebound || !slices.Equal(spec.GPUIndexes, []int{0}) {
		t.Fatalf("leaseCards = %v, %v, %v", rebound, err, spec.GPUIndexes)
	}
	for _, st := range sink.got {
		if len(st.GPUIdents) > 0 {
			t.Fatalf("reported a rebinding for the placed cards: %+v", st)
		}
	}
}

func TestLeaseCardsPassesTheAllocatorsError(t *testing.T) {
	leaser := &fakeLeaser{err: errors.New("Slurm did not start job 20 within 10m0s (AssocGrpGRES)")}
	e, _ := leaseEngine(t, leaser, true, t4(0, "0000:00:07.0"))
	spec := leaseSpec()
	_, err := e.leaseCards(context.Background(), &spec)
	if err == nil || !strings.Contains(err.Error(), "AssocGrpGRES") {
		t.Fatalf("err = %v", err)
	}
	if f := failureOf(err, "", ""); f.Code != proto.ErrorCardLeaseFailed || !strings.Contains(f.Detail, "AssocGrpGRES") {
		t.Fatalf("failure = %+v; want CARD_LEASE_FAILED with the allocator's reason", f)
	}
}

func TestLeaseCardsWithoutALeaserOrCards(t *testing.T) {
	e, _ := leaseEngine(t, nil, true, t4(0, "0000:00:07.0"))
	spec := leaseSpec()
	if rebound, err := e.leaseCards(context.Background(), &spec); err != nil || rebound {
		t.Fatalf("no leaser: %v %v", rebound, err)
	}
	leaser := &fakeLeaser{}
	e, _ = leaseEngine(t, leaser, true)
	cpu := proto.InstanceDesiredState{UUID: "cpu", DesiredState: "running"}
	if _, err := e.leaseCards(context.Background(), &cpu); err != nil || leaser.asked != nil {
		t.Fatalf("an instance without cards asked the leaser: %v %v", err, leaser.asked)
	}
}

// A job the allocator ended under a running instance stops it with a coded
// reason, so the control plane places it again.
func TestLeaseLostFailsTheInstanceWithItsCode(t *testing.T) {
	e, sink := leaseEngine(t, &fakeLeaser{}, true, t4(0, "0000:00:07.0"))
	spec := leaseSpec()
	e.desired[spec.UUID] = spec
	e.LeaseLost(context.Background(), spec.UUID, "Slurm job 5 holding the instance's cards ended (CANCELLED, None)")
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.got) == 0 {
		t.Fatal("nothing reported")
	}
	last := sink.got[len(sink.got)-1]
	if last.State != proto.InstFailed || last.RuntimeFailure == nil || last.RuntimeFailure.Code != proto.ErrorCardLeaseLost || !strings.Contains(last.RuntimeFailure.Detail, "CANCELLED") {
		t.Fatalf("report = %+v; want failed with CARD_LEASE_LOST", last)
	}
	// An instance not running is left alone.
	sink.got = nil
	e.LeaseLost(context.Background(), "unknown", "x")
	if len(sink.got) != 0 {
		t.Fatalf("reported %+v for an unknown instance", sink.got)
	}
}
