package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// pingProber is a prober whose cards own ports: cards 3 and 4 exist, and
// each ping waits for another card's, so overlapping ones are seen.
type pingProber struct {
	gpu.FixedProber
	mu        sync.Mutex
	reachable map[string]bool // "index->ip"
	failing   map[string]error
	inFlight  map[int]int
	overlap   bool
	parallel  int // most cards pinging at once
	timeouts  []time.Duration
	order     []string
	// inventoryReads counts port inventory reads, which a ping never does.
	inventoryReads int
}

// AcceleratorPorts would take every card's tool lock; a ping must not need it.
func (p *pingProber) AcceleratorPorts(context.Context) []proto.AcceleratorPort {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inventoryReads++
	return nil
}

func (p *pingProber) PingAcceleratorPort(_ context.Context, card, target string, timeout time.Duration) (float64, error) {
	index, ok := map[string]int{"npu3": 3, "npu4": 4}[card]
	if !ok {
		return 0, gpu.ErrAcceleratorNoCard
	}
	key := string(rune('0'+index)) + "->" + target
	p.mu.Lock()
	if p.inFlight == nil {
		p.inFlight = map[int]int{}
	}
	p.inFlight[index]++
	if p.inFlight[index] > 1 {
		p.overlap = true
	}
	busy := 0
	for _, n := range p.inFlight {
		if n > 0 {
			busy++
		}
	}
	p.parallel = max(p.parallel, busy)
	p.timeouts = append(p.timeouts, timeout)
	p.order = append(p.order, key)
	// Hold the ping until the other card's is in flight too, so cards pinging
	// together is observed rather than hoped for: a fixed pause lets the first
	// card finish before a starved scheduler even starts the second one. The
	// wait ends as soon as both were seen, and a sequential implementation
	// never sees it, so it fails on parallel below.
	for deadline := time.Now().Add(2 * time.Second); p.parallel < 2 && time.Now().Before(deadline); {
		p.mu.Unlock()
		time.Sleep(time.Millisecond)
		p.mu.Lock()
	}
	// Still take a moment, so two pings on one card would be seen overlapping.
	p.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inFlight[index]--
	if err := p.failing[key]; err != nil {
		return 0, err
	}
	if p.reachable[key] {
		return 0.25, nil
	}
	return 0, gpu.ErrAcceleratorPingTimeout
}

func acceleratorPing(t *testing.T, n *NodeRuntime, params string) []proto.AcceleratorPingResult {
	t.Helper()
	out, err := n.rpcAcceleratorPing(t.Context(), json.RawMessage(params))
	if err != nil {
		t.Fatalf("accelerator.ping %s: %v", params, err)
	}
	// Through the wire form, as the server reads it.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var results []proto.AcceleratorPingResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatal(err)
	}
	return results
}

func TestAcceleratorPingAnswersEachProbeInOrder(t *testing.T) {
	prober := &pingProber{
		FixedProber: gpu.FixedProber{VendorName: "ascend", GPUs: []proto.GPUInfo{{Index: 3}, {Index: 4}}},
		reachable:   map[string]bool{"3->203.0.113.82": true, "4->203.0.113.83": true},
		failing:     map[string]error{"4->203.0.113.99": errors.New("Command execute failed!")},
	}
	n := &NodeRuntime{prober: prober}
	got := acceleratorPing(t, n, `{"timeout_ms":1500,"probes":[
		{"card":"npu3","target":"203.0.113.82"},{"card":"npu4","target":"203.0.113.82"},
		{"card":"npu3","target":"203.0.113.83"},{"card":"npu4","target":"203.0.113.83"},
		{"card":"npu9","target":"203.0.113.82"},{"card":"npu4","target":"203.0.113.99"}]}`)
	want := []proto.AcceleratorPingResult{
		{Card: "npu3", Target: "203.0.113.82", Reachable: true, RTTMS: 0.25},
		{Card: "npu4", Target: "203.0.113.82", Code: proto.AcceleratorPingUnreachable, Error: "no answer within the time limit"},
		{Card: "npu3", Target: "203.0.113.83", Code: proto.AcceleratorPingUnreachable, Error: "no answer within the time limit"},
		{Card: "npu4", Target: "203.0.113.83", Reachable: true, RTTMS: 0.25},
		{Card: "npu9", Target: "203.0.113.82", Code: proto.AcceleratorPingNoCard, Error: "the node reports no card named npu9"},
		{Card: "npu4", Target: "203.0.113.99", Code: proto.AcceleratorPingToolFailed, Error: "Command execute failed!"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("results = %+v\nwant %+v", got, want)
	}
	if prober.overlap {
		t.Fatal("two probes ran on one card at once")
	}
	if prober.inventoryReads != 0 {
		t.Fatalf("a ping read the port inventory %d time(s)", prober.inventoryReads)
	}
	if prober.parallel != 2 {
		t.Fatalf("cards pinging at once = %d, want both", prober.parallel)
	}
	for _, timeout := range prober.timeouts {
		if timeout != 1500*time.Millisecond {
			t.Fatalf("probe time limit = %v", timeout)
		}
	}
	// The exact keys the server reads.
	raw, _ := json.Marshal(want[0])
	if string(raw) != `{"card":"npu3","target":"203.0.113.82","reachable":true,"rtt_ms":0.25}` {
		t.Fatalf("wire = %s", raw)
	}
	raw, _ = json.Marshal(want[1])
	if string(raw) != `{"card":"npu4","target":"203.0.113.82","reachable":false,"code":"unreachable","error":"no answer within the time limit"}` {
		t.Fatalf("wire = %s", raw)
	}
}

func TestAcceleratorPingLimits(t *testing.T) {
	prober := &pingProber{FixedProber: gpu.FixedProber{VendorName: "ascend", GPUs: []proto.GPUInfo{{Index: 3}, {Index: 4}}}}
	n := &NodeRuntime{prober: prober}
	for _, params := range []string{`{`, `{"probes":[]}`, `{}`} {
		if _, err := n.rpcAcceleratorPing(t.Context(), json.RawMessage(params)); err == nil {
			t.Fatalf("params %s were accepted", params)
		}
	}
	many := proto.AcceleratorPingParams{}
	for range acceleratorPingMaxProbes + 1 {
		many.Probes = append(many.Probes, proto.AcceleratorPingProbe{Card: "npu3", Target: "203.0.113.82"})
	}
	raw, _ := json.Marshal(many)
	if _, err := n.rpcAcceleratorPing(t.Context(), raw); err == nil {
		t.Fatal("too many probes were accepted")
	}
	// The time limit defaults to three seconds and is capped.
	acceleratorPing(t, n, `{"probes":[{"card":"npu3","target":"203.0.113.82"}]}`)
	acceleratorPing(t, n, `{"timeout_ms":600000,"probes":[{"card":"npu3","target":"203.0.113.82"}]}`)
	if want := []time.Duration{3 * time.Second, 10 * time.Second}; !reflect.DeepEqual(prober.timeouts, want) {
		t.Fatalf("time limits = %v, want %v", prober.timeouts, want)
	}
}

func TestAcceleratorPingOnANodeWithoutCardPorts(t *testing.T) {
	n := &NodeRuntime{prober: &gpu.FixedProber{VendorName: "nvidia"}}
	got := acceleratorPing(t, n, `{"probes":[{"card":"npu3","target":"203.0.113.82"}]}`)
	if len(got) != 1 || got[0].Reachable || got[0].Code != proto.AcceleratorPingNoCard || got[0].Error == "" {
		t.Fatalf("results = %+v", got)
	}
}
