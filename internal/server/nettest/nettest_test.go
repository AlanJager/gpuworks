package nettest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// fakeAgents mimics the agents' net.bench.* and net.probe RPCs: listeners are
// keyed by token, a probe must present the target listener's token, and the
// wire shapes match internal/agent/app/netprobe.go.
type fakeAgents struct {
	mu          sync.Mutex
	listeners   map[string]string // token -> node uuid
	ports       map[string]int    // node uuid -> port
	stopped     map[string]bool   // node uuid -> stop called
	failStart   map[string]error
	failProbe   map[string]string // "src->dst" -> RPC error
	unreachable map[string]bool   // target uuid -> probe reports unreachable
	sending     map[string]int
	receiving   map[string]int
	maxSend     int
	maxRecv     int
	sourceIPs   map[string]string // "src->dst" -> source_ip
	ipOwner     map[string]string // ip -> uuid
}

func newFakeAgents(ips map[string]string) *fakeAgents {
	f := &fakeAgents{
		listeners: map[string]string{}, ports: map[string]int{}, stopped: map[string]bool{},
		failStart: map[string]error{}, failProbe: map[string]string{}, unreachable: map[string]bool{},
		sending: map[string]int{}, receiving: map[string]int{}, sourceIPs: map[string]string{}, ipOwner: map[string]string{},
	}
	for uuid, ip := range ips {
		f.ipOwner[ip] = uuid
	}
	return f
}

func (f *fakeAgents) call(ctx context.Context, node, method string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	switch method {
	case proto.RPCNetBenchStart:
		var p struct {
			Port  int    `json:"port"`
			Token string `json:"token"`
			TTL   int    `json:"ttl_seconds"`
		}
		_ = json.Unmarshal(raw, &p)
		if err := f.failStart[node]; err != nil {
			return nil, err
		}
		if p.Port != 0 || len(p.Token) != 64 || p.TTL < 5 || p.TTL > 3600 {
			return nil, fmt.Errorf("bad start params %+v", p)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.listeners[p.Token] = node
		f.ports[node] = 40000 + len(f.ports)
		return json.Marshal(map[string]int{"port": f.ports[node]})
	case proto.RPCNetBenchStop:
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped[node] = true
		return json.Marshal(map[string]bool{"stopped": true})
	case proto.RPCNetProbe:
		var p struct {
			Target    string `json:"target"`
			Token     string `json:"token"`
			Bandwidth bool   `json:"bandwidth"`
			Seconds   int    `json:"seconds"`
			SourceIP  string `json:"source_ip"`
		}
		_ = json.Unmarshal(raw, &p)
		host, _, err := net.SplitHostPort(p.Target)
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		target := f.ipOwner[host]
		key := node + "->" + target
		f.sourceIPs[key] = p.SourceIP
		f.sending[node]++
		f.receiving[target]++
		f.maxSend = max(f.maxSend, f.sending[node])
		f.maxRecv = max(f.maxRecv, f.receiving[target])
		tokenOwner := f.listeners[p.Token]
		rpcErr := f.failProbe[key]
		unreachable := f.unreachable[target]
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		f.mu.Lock()
		f.sending[node]--
		f.receiving[target]--
		f.mu.Unlock()
		if rpcErr != "" {
			return nil, errors.New(rpcErr)
		}
		if tokenOwner != target {
			return json.Marshal(map[string]any{"reachable": false, "error": "rejected authentication"})
		}
		if unreachable {
			return json.Marshal(map[string]any{"reachable": false, "error": "dial tcp " + p.Target + ": i/o timeout"})
		}
		out := map[string]any{"reachable": true, "rtt_ms_p50": 0.2, "rtt_ms_p99": 0.5, "bandwidth_tested": p.Bandwidth}
		if p.Bandwidth {
			out["gbps"] = 23.4
		}
		return json.Marshal(out)
	}
	return nil, fmt.Errorf("unexpected method %s", method)
}

func endpoints(ips map[string]string, uuids ...string) []Endpoint {
	var eps []Endpoint
	for _, uuid := range uuids {
		eps = append(eps, Endpoint{NodeUUID: uuid, IP: ips[uuid]})
	}
	return eps
}

func TestRunProbesEveryOrderedPairWithBoundedFanOut(t *testing.T) {
	ips := map[string]string{"a": "10.30.1.1", "b": "10.30.1.2", "c": "10.30.1.3", "d": "10.30.1.4"}
	f := newFakeAgents(ips)
	pairs := Run(context.Background(), f.call, endpoints(ips, "a", "b", "c", "d"), Options{Bandwidth: true, Seconds: 1})
	if len(pairs) != 12 || PairCount(PatternMesh, 4) != 12 {
		t.Fatalf("pairs = %d, want 12", len(pairs))
	}
	for _, p := range pairs {
		if p.Status(true) != StatusPassed || p.Gbps != 23.4 {
			t.Fatalf("pair = %+v", p)
		}
		if got := f.sourceIPs[p.SourceUUID+"->"+p.TargetUUID]; got != ips[p.SourceUUID] {
			t.Fatalf("%s->%s source_ip = %q", p.SourceUUID, p.TargetUUID, got)
		}
	}
	if pairs[0].SourceUUID != "a" || pairs[0].TargetUUID != "b" || pairs[11].SourceUUID != "d" || pairs[11].TargetUUID != "c" {
		t.Fatalf("order = %+v ... %+v", pairs[0], pairs[11])
	}
	if f.maxSend != 1 || f.maxRecv != 1 {
		t.Fatalf("fan-out send=%d recv=%d, want one stream each way per node", f.maxSend, f.maxRecv)
	}
	for _, uuid := range []string{"a", "b", "c", "d"} {
		if !f.stopped[uuid] {
			t.Fatalf("listener on %s was not stopped", uuid)
		}
	}
}

// A ring probes each node's next node in the given order, the last the
// first: every node sends once and receives once.
func TestRunRingProbesEachNodesSuccessor(t *testing.T) {
	ips := map[string]string{"a": "10.30.1.1", "b": "10.30.1.2", "c": "10.30.1.3", "d": "10.30.1.4"}
	f := newFakeAgents(ips)
	pairs := Run(context.Background(), f.call, endpoints(ips, "c", "a", "d", "b"), Options{Pattern: PatternRing})
	var got []string
	for _, p := range pairs {
		got = append(got, p.SourceUUID+"->"+p.TargetUUID)
		if p.Status(false) != StatusPassed {
			t.Fatalf("pair = %+v", p)
		}
	}
	if want := []string{"a->d", "b->c", "c->a", "d->b"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("ring pairs = %v, want %v", got, want)
	}
	if PairCount(PatternRing, 4) != 4 || Rounds(PatternRing, 2) != 1 || PairCount(PatternRing, 2) != 2 || Rounds(PatternMesh, 1) != 0 {
		t.Fatal("pair counts")
	}
}

func TestRunReportsEveryFailureOnTheAffectedPairs(t *testing.T) {
	ips := map[string]string{"a": "10.30.1.1", "b": "10.30.1.2", "c": "10.30.1.3", "e": "10.30.1.5"}
	f := newFakeAgents(ips)
	f.failStart["c"] = errors.New("node c is not connected")
	f.failProbe["a->b"] = "rpc timeout"
	f.unreachable["e"] = true
	eps := []Endpoint{
		{NodeUUID: "a", IP: ips["a"]}, {NodeUUID: "b", IP: ips["b"]}, {NodeUUID: "c", IP: ips["c"]},
		{NodeUUID: "d"}, // nothing on the node matched the network
		{NodeUUID: "e", IP: ips["e"]},
		{NodeUUID: "f", IP: "10.30.1.6", Unavailable: CodeOffline},
		{NodeUUID: "g", Unavailable: CodeDeleted},
	}
	pairs := Run(context.Background(), f.call, eps, Options{})
	byKey := map[string]Pair{}
	for _, p := range pairs {
		byKey[p.SourceUUID+"->"+p.TargetUUID] = p
	}
	for key, want := range map[string]struct{ code, side, err string }{
		"a->b": {CodeRPCFailed, SideSource, "source: rpc timeout"},
		"a->c": {CodeListenerFailed, SideTarget, "target: node c is not connected"},
		"a->d": {CodeNoAddress, SideTarget, "target has no address on this network"},
		"d->a": {CodeNoAddress, SideSource, "source has no address on this network"},
		"a->e": {CodeProbeFailed, "", "dial tcp 10.30.1.5"},
		"a->f": {CodeOffline, SideTarget, "target is offline"},
		"f->a": {CodeOffline, SideSource, "source is offline"},
		"g->a": {CodeDeleted, SideSource, "source was deleted"},
	} {
		p := byKey[key]
		if p.Status(false) != StatusFailed || p.Code != want.code || p.Side != want.side || !strings.Contains(p.Error, want.err) {
			t.Fatalf("%s = %+v, want %+v", key, p, want)
		}
	}
	if p := byKey["b->a"]; p.Status(false) != StatusPassed || p.Gbps != 0 {
		t.Fatalf("healthy pair without bandwidth = %+v", p)
	}
	if f.stopped["c"] || f.stopped["d"] || f.stopped["f"] {
		t.Fatalf("stopped listeners that never started: %v", f.stopped)
	}
	if !f.stopped["a"] || !f.stopped["e"] {
		t.Fatalf("started listeners not stopped: %v", f.stopped)
	}
}

func TestRunHandlesMalformedAgentReplies(t *testing.T) {
	call := func(ctx context.Context, node, method string, params any) (json.RawMessage, error) {
		switch method {
		case proto.RPCNetBenchStart:
			if node == "b" {
				return json.RawMessage(`{"port":0}`), nil
			}
			return json.RawMessage(`{"port":41000}`), nil
		case proto.RPCNetProbe:
			return json.RawMessage(`{not json`), nil
		}
		return json.RawMessage(`{}`), nil
	}
	pairs := Run(context.Background(), call, []Endpoint{{NodeUUID: "a", IP: "10.0.0.1"}, {NodeUUID: "b", IP: "10.0.0.2"}}, Options{})
	if len(pairs) != 2 || !strings.Contains(pairs[0].Error, "listener reported port 0") || !strings.Contains(pairs[1].Error, "decode probe response") {
		t.Fatalf("pairs = %+v", pairs)
	}
}

func TestRunBandwidthFailureFailsThePair(t *testing.T) {
	call := func(ctx context.Context, node, method string, params any) (json.RawMessage, error) {
		if method == proto.RPCNetProbe {
			return json.RawMessage(`{"reachable":true,"rtt_ms_p50":0.2,"bandwidth_error":"stream reset"}`), nil
		}
		return json.RawMessage(`{"port":41000}`), nil
	}
	pairs := Run(context.Background(), call, []Endpoint{{NodeUUID: "a", IP: "10.0.0.1"}, {NodeUUID: "b", IP: "10.0.0.2"}}, Options{Bandwidth: true})
	if p := pairs[0]; p.Code != CodeBandwidthFailed || p.Error != "stream reset" || p.Status(true) != StatusFailed || !p.Reachable {
		t.Fatalf("pair = %+v", p)
	}
}

func TestRunWithFewerThanTwoNodes(t *testing.T) {
	calls := 0
	call := func(context.Context, string, string, any) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"port":1}`), nil
	}
	if pairs := Run(context.Background(), call, []Endpoint{{NodeUUID: "a", IP: "10.0.0.1"}}, Options{}); len(pairs) != 0 {
		t.Fatalf("pairs = %+v", pairs)
	}
	if pairs := Run(context.Background(), call, nil, Options{}); len(pairs) != 0 {
		t.Fatalf("pairs = %+v", pairs)
	}
}

func TestPairStatus(t *testing.T) {
	good := RailResult{Rail: "a:1", Gbps: 24}
	bad := RailResult{Rail: "b:1", Code: CodeToolFailed, Error: "x"}
	for _, c := range []struct {
		pair      Pair
		bandwidth bool
		want      string
	}{
		{Pair{Reachable: true, Gbps: 1}, true, StatusPassed},
		{Pair{Reachable: true}, true, StatusFailed},
		{Pair{Reachable: true}, false, StatusPassed},
		{Pair{Reachable: false}, false, StatusFailed},
		{Pair{Reachable: true, Code: CodeProbeFailed}, false, StatusFailed},
		{Pair{Reachable: true, Rails: []RailResult{good, good}}, true, StatusPassed},
		{Pair{Reachable: true, Rails: []RailResult{good, bad}}, true, StatusPartial},
		{Pair{Reachable: true, Rails: []RailResult{bad}}, true, StatusFailed},
		{Pair{Reachable: true, Rails: []RailResult{{Rail: "z:1"}}}, true, StatusFailed},
		// Runs stored before codes carried only an error text.
		{Pair{Reachable: false, Error: "target has no bootstrap address on this network"}, true, StatusFailed},
		{Pair{Reachable: true, Error: "b:1: x", Rails: []RailResult{good, {Rail: "b:1", Error: "x"}}}, true, StatusPartial},
	} {
		if got := c.pair.Status(c.bandwidth); got != c.want {
			t.Errorf("Status(%+v, %v) = %s, want %s", c.pair, c.bandwidth, got, c.want)
		}
	}
}
