package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/perftest"
	"gpuworks.ai/gpuworks/internal/proto"
)

const benchReport = ` #bytes     #iterations    BW peak[Gb/sec]    BW average[Gb/sec]   MsgRate[Mpps]
 65536      3354100          0.00               361.50		   0.689
`

// benchCalls records the perftest commands a fake runner was asked to run.
// Runner.Run measures rails concurrently, so recording must be locked.
type benchCalls struct {
	mu    sync.Mutex
	calls []string
}

func (b *benchCalls) add(call string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, call)
}

func (b *benchCalls) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func benchNode(available bool) (*NodeRuntime, *benchCalls) {
	calls := &benchCalls{}
	n := &NodeRuntime{log: slog.Default()}
	n.perftest = perftest.Runner{
		Locate: func() (string, error) {
			if !available {
				return "", perftest.ErrToolMissing
			}
			return "/usr/bin/ib_write_bw", nil
		},
		Start: func(_ context.Context, name string, args ...string) (func() error, error) {
			calls.add("serve " + strings.Join(args, " "))
			return func() error { return nil }, nil
		},
		Output: func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
			calls.add("run " + strings.Join(args, " "))
			return []byte(benchReport), nil, nil
		},
	}
	return n, calls
}

func TestRDMABenchRPCs(t *testing.T) {
	n, calls := benchNode(true)
	params, _ := json.Marshal(proto.RDMABenchParams{Rails: []string{"mlx5_0:1", "mlx5_1:1"}, BasePort: 18515, Seconds: 3, Target: "10.96.0.48"})
	served, err := n.rpcRDMABenchServe(params)
	if err != nil {
		t.Fatal(err)
	}
	if wire, _ := json.Marshal(served); string(wire) != `{}` {
		t.Fatalf("serve result = %s", wire)
	}
	res, err := n.rpcRDMABenchRun(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	want := []proto.RDMABenchRail{{Rail: "mlx5_0:1", Gbps: 361.5}, {Rail: "mlx5_1:1", Gbps: 361.5}}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("results = %#v", res)
	}
	if got := calls.snapshot(); len(got) != 4 {
		t.Fatalf("calls = %v", got)
	}

	for _, bad := range []proto.RDMABenchParams{
		{BasePort: 18515, Seconds: 3},
		{Rails: []string{"mlx5_0:1"}, BasePort: 80, Seconds: 3},
		{Rails: []string{"mlx5_0:1"}, BasePort: 18515, Seconds: 0},
		{Rails: []string{"mlx5_0:x"}, BasePort: 18515, Seconds: 3},
	} {
		raw, _ := json.Marshal(bad)
		if _, err := n.rpcRDMABenchServe(raw); err == nil {
			t.Fatalf("serve accepted %+v", bad)
		}
	}
	noTarget, _ := json.Marshal(proto.RDMABenchParams{Rails: []string{"mlx5_0:1"}, BasePort: 18515, Seconds: 3, Target: "node-b"})
	if _, err := n.rpcRDMABenchRun(context.Background(), noTarget); err == nil || !strings.Contains(err.Error(), "IP address") {
		t.Fatalf("hostname target err = %v", err)
	}
	if _, err := n.rpcRDMABenchServe(json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed params accepted")
	}

	// A node without the tool answers per rail, so the server can say which
	// node lacks it instead of failing the whole RPC.
	missing, _ := benchNode(false)
	served, err = missing.rpcRDMABenchServe(params)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(served)
	if want := `{"failed":[{"rail":"mlx5_0:1","gbps":0,"code":"tool_missing","error":"ib_write_bw is not installed (install perftest)"},{"rail":"mlx5_1:1","gbps":0,"code":"tool_missing","error":"ib_write_bw is not installed (install perftest)"}]}`; string(wire) != want {
		t.Fatalf("serve result = %s", wire)
	}
	ran, err := missing.rpcRDMABenchRun(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if rails := ran.([]proto.RDMABenchRail); len(rails) != 2 || rails[1].Code != proto.RDMABenchToolMissing {
		t.Fatalf("run result = %#v", ran)
	}
}

func TestNetworkToolsReportsRunnableTool(t *testing.T) {
	if n, _ := benchNode(true); !reflect.DeepEqual(n.NetworkTools(), []string{"ib_write_bw"}) {
		t.Fatalf("tools = %v", n.NetworkTools())
	}
	if n, _ := benchNode(false); n.NetworkTools() != nil {
		t.Fatalf("tools = %v", n.NetworkTools())
	}
}
