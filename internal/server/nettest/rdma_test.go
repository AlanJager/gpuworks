package nettest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func railNames(rails []Rail) []string {
	var out []string
	for _, r := range rails {
		out = append(out, r.Name)
	}
	return out
}

func TestPairRails(t *testing.T) {
	// Point-to-point links, listed in a different order on each side: the
	// subnet decides, not the position.
	src := []Rail{{"s0:1", "100.64.29.209/30"}, {"s2:1", "100.64.29.213/30"}, {"s9:1", "100.64.0.1/30"}}
	dst := []Rail{{"t2:1", "100.64.29.214/30"}, {"t0:1", "100.64.29.210/30"}}
	pairs, lonelySrc, lonelyDst := pairRails(src, dst)
	var got []string
	for _, p := range pairs {
		got = append(got, p[0].Name+"="+p[1].Name)
	}
	if want := []string{"s0:1=t0:1", "s2:1=t2:1"}; !reflect.DeepEqual(got, want) || len(lonelyDst) != 0 || !reflect.DeepEqual(railNames(lonelySrc), []string{"s9:1"}) {
		t.Fatalf("pairs %v lonely %v %v", got, lonelySrc, lonelyDst)
	}

	// InfiniBand without addresses, and a flat subnet: in order.
	ib := []Rail{{"mlx5_0:1", ""}, {"mlx5_1:1", ""}}
	pairs, _, lonelyDst = pairRails(ib, append(ib, Rail{"mlx5_2:1", ""}))
	if len(pairs) != 2 || pairs[1][1].Name != "mlx5_1:1" || !reflect.DeepEqual(railNames(lonelyDst), []string{"mlx5_2:1"}) {
		t.Fatalf("ib pairs %v lonely %v", pairs, lonelyDst)
	}
	flat := []Rail{{"a:1", "10.0.0.1/16"}, {"b:1", "10.0.0.2/16"}}
	pairs, _, _ = pairRails(flat, []Rail{{"c:1", "10.0.1.1/16"}, {"d:1", "10.0.1.2/16"}})
	if pairs[0][1].Name != "c:1" || pairs[1][1].Name != "d:1" {
		t.Fatalf("flat subnet pairs %v", pairs)
	}
	// A matched rail is not handed out again to an unmatched one.
	pairs, _, _ = pairRails([]Rail{{"x:1", ""}, {"y:1", "10.9.0.1/30"}}, []Rail{{"p:1", "10.9.0.2/30"}, {"q:1", ""}})
	if pairs[0][1].Name != "q:1" || pairs[1][1].Name != "p:1" {
		t.Fatalf("mixed pairs %v", pairs)
	}
}

// rdmaAgents mimics rdma.bench.serve and rdma.bench.run as agents answer
// them; failing rails are keyed "node/rail".
type rdmaAgents struct {
	mu         sync.Mutex
	serveErr   map[string]error
	unserved   map[string]proto.RDMABenchRail
	runFailure map[string]proto.RDMABenchRail
	oldAgent   map[string]bool
	served     map[string][]proto.RDMABenchParams
	ran        map[string][]proto.RDMABenchParams
}

func newRDMAAgents() *rdmaAgents {
	return &rdmaAgents{serveErr: map[string]error{}, unserved: map[string]proto.RDMABenchRail{}, runFailure: map[string]proto.RDMABenchRail{},
		oldAgent: map[string]bool{}, served: map[string][]proto.RDMABenchParams{}, ran: map[string][]proto.RDMABenchParams{}}
}

func (f *rdmaAgents) call(_ context.Context, node, method string, params any) (json.RawMessage, error) {
	p := params.(proto.RDMABenchParams)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case proto.RPCRDMABenchServe:
		if err := f.serveErr[node]; err != nil {
			return nil, err
		}
		f.served[node] = append(f.served[node], p)
		if f.oldAgent[node] {
			return json.RawMessage(`{"serving":true}`), nil
		}
		var served proto.RDMABenchServed
		for _, r := range p.Rails {
			if failed, ok := f.unserved[node+"/"+r]; ok {
				served.Failed = append(served.Failed, failed)
			}
		}
		return json.Marshal(served)
	case proto.RPCRDMABenchRun:
		f.ran[node] = append(f.ran[node], p)
		var rails []proto.RDMABenchRail
		for _, r := range p.Rails {
			rail := proto.RDMABenchRail{Rail: r, Gbps: 24.4}
			if failed, ok := f.runFailure[node+"/"+r]; ok {
				rail = failed
			}
			rails = append(rails, rail)
		}
		return json.Marshal(rails)
	}
	return nil, fmt.Errorf("unexpected %s", method)
}

func TestRunRDMAToolReportsEachRail(t *testing.T) {
	f := newRDMAAgents()
	f.runFailure["b/rb1:1"] = proto.RDMABenchRail{Rail: "rb1:1", Code: proto.RDMABenchToolFailed, Error: "Failed to modify QP 92 to RTR"}
	f.unserved["a/ra1:1"] = proto.RDMABenchRail{Rail: "ra1:1", Code: proto.RDMABenchNoRoCEv2GID, Error: "RoCE port has no IPv4 RoCE v2 GID"}
	eps := []Endpoint{
		{NodeUUID: "a", IP: "100.64.29.209", Rails: []Rail{{"ra0:1", "100.64.29.209/30"}, {"ra1:1", "100.64.29.213/30"}}},
		{NodeUUID: "b", IP: "100.64.29.210", Rails: []Rail{{"rb1:1", "100.64.29.214/30"}, {"rb0:1", "100.64.29.210/30"}}},
	}
	pairs := Run(context.Background(), f.call, eps, Options{Mode: ModeRDMATool, Bandwidth: true, Seconds: 2})
	ab, ba := pairs[0], pairs[1]
	wantAB := []RailResult{
		{Rail: "ra0:1", SourceIP: "100.64.29.209", TargetRail: "rb0:1", TargetIP: "100.64.29.210", Gbps: 24.4},
		{Rail: "ra1:1", SourceIP: "100.64.29.213", TargetRail: "rb1:1", TargetIP: "100.64.29.214", Gbps: 24.4},
	}
	if !reflect.DeepEqual(ab.Rails, wantAB) || ab.Gbps != 48.8 || ab.Status(true) != StatusPassed || ab.Target != "100.64.29.210" {
		t.Fatalf("a->b = %+v", ab)
	}
	// b's rb1 fails on its own run; a could not serve ra1, which is the real
	// reason for that rail however the client failed.
	wantBA := []RailResult{
		{Rail: "rb1:1", SourceIP: "100.64.29.214", TargetRail: "ra1:1", TargetIP: "100.64.29.213", Code: proto.RDMABenchNoRoCEv2GID, Side: SideTarget, Error: "RoCE port has no IPv4 RoCE v2 GID"},
		{Rail: "rb0:1", SourceIP: "100.64.29.210", TargetRail: "ra0:1", TargetIP: "100.64.29.209", Gbps: 24.4},
	}
	if !reflect.DeepEqual(ba.Rails, wantBA) || ba.Status(true) != StatusPartial || ba.Code != "" {
		t.Fatalf("b->a = %+v", ba)
	}
	// The server was asked for the rails in pair order.
	if got := f.served["a"][0].Rails; !reflect.DeepEqual(got, []string{"ra1:1", "ra0:1"}) {
		t.Fatalf("a served %v", got)
	}
	if got := f.ran["b"][0]; !reflect.DeepEqual(got.Rails, []string{"rb1:1", "rb0:1"}) || got.Target != "100.64.29.209" {
		t.Fatalf("b ran %+v", got)
	}
}

func TestRunRDMAToolFailures(t *testing.T) {
	f := newRDMAAgents()
	f.serveErr["c"] = errors.New("node c is not connected")
	f.oldAgent["a"] = true
	f.runFailure["b/mlx5_1:1"] = proto.RDMABenchRail{Rail: "mlx5_1:1", Code: proto.RDMABenchToolMissing, Error: "ib_write_bw is not installed (install perftest)"}
	ib := func(n int) []Rail {
		var rails []Rail
		for i := 0; i < n; i++ {
			rails = append(rails, Rail{Name: fmt.Sprintf("mlx5_%d:1", i)})
		}
		return rails
	}
	eps := []Endpoint{
		{NodeUUID: "a", IP: "10.96.0.1", Rails: ib(3)},
		{NodeUUID: "b", IP: "10.96.0.2", Rails: ib(2)},
		{NodeUUID: "c", IP: "10.96.0.3", Rails: ib(2)},
		{NodeUUID: "d", IP: "10.96.0.4", RDMAUnavailable: CodeToolMissing},
		{NodeUUID: "e", RDMAUnavailable: ""},
	}
	pairs := Run(context.Background(), f.call, eps, Options{Mode: ModeRDMATool, Bandwidth: true, Seconds: 2})
	byKey := map[string]Pair{}
	for _, p := range pairs {
		byKey[p.SourceUUID+"->"+p.TargetUUID] = p
	}
	// a has a third rail b lacks; the older agent a still serves.
	ab := byKey["a->b"]
	if ab.Status(true) != StatusPartial || len(ab.Rails) != 3 || ab.Rails[2].Code != CodeNoPeerRail || ab.Rails[2].Side != SideSource || ab.Gbps != 48.8 {
		t.Fatalf("a->b = %+v", ab)
	}
	ba := byKey["b->a"]
	if ba.Rails[1].Code != CodeToolMissing || ba.Rails[1].Side != SideSource || ba.Rails[2].Code != CodeNoPeerRail || ba.Rails[2].Side != SideTarget || ba.Rails[2].TargetRail != "mlx5_2:1" {
		t.Fatalf("b->a = %+v", ba)
	}
	for key, want := range map[string]struct{ code, side string }{
		"a->c": {CodeRPCFailed, SideTarget},
		"d->a": {CodeToolMissing, SideSource},
		"a->d": {CodeToolMissing, SideTarget},
		"a->e": {CodeNoAddress, SideTarget},
	} {
		if p := byKey[key]; p.Code != want.code || p.Side != want.side || p.Status(true) != StatusFailed {
			t.Fatalf("%s = %+v", key, p)
		}
	}
	// Each round uses its own port block.
	blocks := map[int]bool{}
	for _, ps := range f.served {
		for _, p := range ps {
			if (p.BasePort-rdmaBasePort)%32 != 0 {
				t.Fatalf("unexpected base port %d", p.BasePort)
			}
			blocks[p.BasePort] = true
		}
	}
	if len(blocks) < 2 {
		t.Fatalf("rounds reused one port block: %v", blocks)
	}
}

func TestRunRDMAToolMalformedRun(t *testing.T) {
	call := func(_ context.Context, node, method string, params any) (json.RawMessage, error) {
		if method == proto.RPCRDMABenchServe {
			return json.RawMessage(`{}`), nil
		}
		if node == "a" {
			return nil, errors.New("rpc timeout")
		}
		return json.RawMessage(`{bad`), nil
	}
	eps := []Endpoint{{NodeUUID: "a", IP: "10.0.0.1", Rails: []Rail{{Name: "m:1"}}}, {NodeUUID: "b", IP: "10.0.0.2", Rails: []Rail{{Name: "m:1"}}}}
	pairs := Run(context.Background(), call, eps, Options{Mode: ModeRDMATool})
	if pairs[0].Code != CodeRPCFailed || pairs[0].Side != SideSource || pairs[0].Error != "source: rpc timeout" {
		t.Fatalf("a->b = %+v", pairs[0])
	}
	if pairs[1].Code != CodeRPCFailed || pairs[1].Status(true) != StatusFailed {
		t.Fatalf("b->a = %+v", pairs[1])
	}
}

func TestFallbackText(t *testing.T) {
	for _, c := range []struct{ code, side, detail, want string }{
		{CodeNoHCA, SideSource, "", "source has no active RDMA ports on this network"},
		{CodeToolMissing, SideTarget, "librdmacm missing", "target has no ib_write_bw (install perftest)"},
		{CodeNoPeerRail, SideTarget, "", "no source rail to pair with"},
		{CodeOffline, "", "", "node is offline"},
		{CodeProbeFailed, "", "i/o timeout", "i/o timeout"},
	} {
		if got := fallback(c.code, c.side, c.detail); got != c.want {
			t.Errorf("fallback(%s,%s,%s) = %q, want %q", c.code, c.side, c.detail, got, c.want)
		}
	}
}
