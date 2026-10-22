package pdverify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeSource []Candidate

func (f fakeSource) Pending(context.Context) ([]Candidate, error) { return f, nil }

type fakeProber map[string]Outcome

func (f fakeProber) Probe(_ context.Context, _ uint, prefill string) (Outcome, error) {
	o, ok := f[prefill]
	if !ok {
		return Outcome{}, errors.New("not routable")
	}
	return o, nil
}

type fakeCounters struct {
	received float64
	known    bool
	asked    []string
}

func (f *fakeCounters) RDMAReceived(_ context.Context, node string, _, _ time.Time) (float64, bool, error) {
	f.asked = append(f.asked, node)
	return f.received, f.known, nil
}

type record struct {
	uuid, code string
	params     map[string]any
}
type fakeRecorder struct{ records []record }

func (f *fakeRecorder) Record(uuid, code string, params map[string]any) {
	f.records = append(f.records, record{uuid, code, params})
}

func run(t *testing.T, candidates []Candidate, prober fakeProber, counters *fakeCounters) []record {
	t.Helper()
	recorder := &fakeRecorder{}
	v := &Verifier{Source: fakeSource(candidates), Prober: prober, Counters: counters, Recorder: recorder,
		sleep: func(context.Context, time.Duration) bool { return true }}
	v.Tick(context.Background())
	return recorder.records
}

func TestVerifierConclusions(t *testing.T) {
	now := time.Now()
	moved := Outcome{DecodeUUID: "d1", DecodeNodes: []string{"n2"}, PrefillNodes: []string{"n1"}, Bytes: 1e8, StartedAt: now, EndedAt: now}
	cases := []struct {
		name      string
		network   string
		outcome   Outcome
		counters  fakeCounters
		code      string
		transport string
	}{
		{"same node", "host_rdma", Outcome{DecodeUUID: "d1", DecodeNodes: []string{"n1"}, PrefillNodes: []string{"n1"}, Bytes: 1e8}, fakeCounters{}, EventVerified, TransportLocal},
		{"ethernet", "host_ethernet", moved, fakeCounters{}, EventVerified, TransportTCP},
		// The cards' own ports are outside every host counter, whatever the
		// host's RDMA counters happen to read.
		{"accelerator cards", "accelerator_rdma", moved, fakeCounters{received: 9e7, known: true}, EventVerified, TransportUnmeasured},
		{"management", "", moved, fakeCounters{}, EventVerified, TransportTCP},
		{"rdma carried it", "host_rdma", moved, fakeCounters{received: 9e7, known: true}, EventVerified, TransportRDMA},
		{"rdma silent", "host_rdma", moved, fakeCounters{received: 1e6, known: true}, EventNotRDMA, ""},
		{"rdma counters missing", "host_rdma", moved, fakeCounters{}, EventNotRDMA, ""},
		{"multi-node decode leaves the node", "host_ethernet", Outcome{DecodeUUID: "d1", DecodeNodes: []string{"n1", "n2"}, PrefillNodes: []string{"n1"}, Bytes: 1e8}, fakeCounters{}, EventVerified, TransportTCP},
		{"multi-node decode over rdma", "host_rdma", Outcome{DecodeUUID: "d1", DecodeNodes: []string{"n1", "n2"}, PrefillNodes: []string{"n1"}, Bytes: 1e8}, fakeCounters{received: 3e7, known: true}, EventVerified, TransportRDMA},
		{"failed", "host_rdma", Outcome{DecodeUUID: "d1", Failed: true, Reason: "transfer_failed"}, fakeCounters{}, EventFailed, ""},
		{"nothing moved", "host_ethernet", Outcome{DecodeUUID: "d1"}, fakeCounters{}, EventFailed, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			counters := c.counters
			records := run(t, []Candidate{{DeploymentID: 1, PrefillUUID: "p1", NetworkType: c.network}}, fakeProber{"p1": c.outcome}, &counters)
			if len(records) != 1 || records[0].uuid != "p1" || records[0].code != c.code {
				t.Fatalf("records = %+v", records)
			}
			if c.transport != "" && records[0].params["transport"] != c.transport {
				t.Fatalf("transport = %v", records[0].params)
			}
			if c.code == EventNotRDMA && len(counters.asked) != 1 {
				t.Fatal("RDMA counters were not read on the decoder's node")
			}
		})
	}
	if records := run(t, []Candidate{{PrefillUUID: "p1", NetworkType: "host_rdma"}}, fakeProber{"p1": {Failed: true}}, &fakeCounters{}); records[0].params["reason"] != "no_transfer" && records[0].params["reason"] != "" {
		t.Fatalf("reason = %v", records[0].params)
	}
}

func TestVerifierDefersWhatItCannotProbe(t *testing.T) {
	records := run(t, []Candidate{{PrefillUUID: "gone"}, {PrefillUUID: "p1", NetworkType: "host_ethernet"}}, fakeProber{"p1": {DecodeNodes: []string{"a"}, PrefillNodes: []string{"b"}, Bytes: 10}}, &fakeCounters{})
	if len(records) != 1 || records[0].uuid != "p1" {
		t.Fatalf("records = %+v", records)
	}
}

func TestVerifierUnmeasuredHandoff(t *testing.T) {
	records := run(t, []Candidate{{PrefillUUID: "p1", NetworkType: "host_rdma"}}, fakeProber{"p1": {DecodeUUID: "d1", Unmeasured: true}}, &fakeCounters{})
	if len(records) != 1 || records[0].code != EventVerified || records[0].params["transport"] != TransportUnmeasured {
		t.Fatalf("records = %+v", records)
	}
	records = run(t, []Candidate{{PrefillUUID: "p1"}}, fakeProber{"p1": {Unmeasured: true, Failed: true, Reason: "decode_failed"}}, &fakeCounters{})
	if records[0].code != EventFailed {
		t.Fatalf("a failed unmeasured handoff = %+v", records)
	}
}

func TestIsConclusionNamesExactlyTheVerifierEvents(t *testing.T) {
	for _, code := range []string{EventVerified, EventNotRDMA, EventFailed} {
		if !IsConclusion(code) {
			t.Fatalf("%s is not a conclusion", code)
		}
	}
	for _, code := range []string{"", "PD_HANDOFF_", "PD_GROUP_RESTART_REQUIRED", "INSTANCE_STARTED"} {
		if IsConclusion(code) {
			t.Fatalf("%q is a conclusion", code)
		}
	}
}

type countingSource struct {
	mu    sync.Mutex
	calls int
}

func (s *countingSource) Pending(context.Context) ([]Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return nil, nil
}

// A wake starts the next pass only after WakeGap: an instance storm costs one
// pass per gap, not one per event.
func TestVerifierCoalescesWakesByGap(t *testing.T) {
	source := &countingSource{}
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slept := make(chan time.Duration, 4)
	v := &Verifier{Source: source, Interval: time.Hour, Wake: wake, WakeGap: 7 * time.Second,
		sleep: func(ctx context.Context, d time.Duration) bool { slept <- d; return ctx.Err() == nil }}
	done := make(chan struct{})
	go func() { v.Run(ctx); close(done) }()
	wake <- struct{}{}
	select {
	case d := <-slept:
		if d != 7*time.Second {
			t.Fatalf("waited %s after a wake, want the gap", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a wake did not wait out the gap")
	}
	cancel()
	<-done
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.calls < 1 || source.calls > 2 {
		t.Fatalf("passes = %d", source.calls)
	}
}
