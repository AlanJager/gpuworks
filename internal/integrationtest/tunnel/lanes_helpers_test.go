package tunnel_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// bulkTarget identifies a deployed bulk-engine model reachable through /v1.
type bulkTarget struct {
	key    string
	served string
}

// deployBulk installs the bulk engine handler on the node's fake runtime,
// deploys a model served as "bulk-model", and returns the /v1 target.
func deployBulk(t *testing.T, env *testenv.Env, node *testenv.Node) bulkTarget {
	t.Helper()
	node.Driver.SetEngineHandler(bulkEngineHandler)
	const served = "bulk-model"
	deployBulkModel(t, env, node, served)
	return bulkTarget{key: apiKey(t, env), served: served}
}

// streamProxyBulk downloads size bytes through the proxy tunnel and returns the
// byte count actually received; any transport error fails the test.
func streamProxyBulk(t *testing.T, env *testenv.Env, tgt bulkTarget, size int) int64 {
	t.Helper()
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := bulkGet(client, env.URL(), tgt.key, tgt.served, 3, size)
	if err != nil {
		t.Fatalf("bulk request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("bulk status %d: %s", resp.StatusCode, b)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("bulk read: %v", err)
	}
	return n
}

// streamProxyBulkCtx is the saturation variant: it downloads size bytes until
// done or ctx is cancelled and ignores errors (cancellation is expected).
func streamProxyBulkCtx(ctx context.Context, env *testenv.Env, tgt bulkTarget, size int) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		env.URL()+"/v1/bulk?model="+tgt.served+"&bytes="+itoa(size)+"&seed=9", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+tgt.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b[i:])
}

// skipUnlessPerf gates the wall-clock throughput and latency assertions.
//
// These measure an in-process network simulation, so their numbers track
// whatever else the host is doing rather than the code under test. The same
// ctrl-latency case measured 22 ms, 31 ms and 50 ms on an idle box (the last
// with all 12 cores saturated) and still blew past its 500 ms ceiling when the
// full suite ran beside a real CI job on the same machine; the legacy-lane
// throughput case read 396 Mbps and 476 Mbps on consecutive runs in the same
// container. A floor or ceiling that moves with the host is a coin flip in CI,
// not a regression signal.
//
// They stay compiled and runnable — GPUWORKS_TEST_PERF=1 on an otherwise idle
// machine — so they cannot rot, but they do not gate the default suite:
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v ./internal/integrationtest/tunnel/ -run 'TestLanes_(CtrlLatencyUnderDataSaturation|Throughput_20msRTT|LegacyAgentSingleLane)'
//
// The functional assertions around them (lane routing, data-lane attach and
// fallback, byte integrity, reconnect) are unconditional and carry the real
// coverage.
func skipUnlessPerf(t *testing.T) {
	t.Helper()
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("wall-clock perf assertion; set GPUWORKS_TEST_PERF=1 on an idle host to run")
	}
}

// ctrlLaneWindowBytes mirrors hub.ctrlLaneWindow, the fixed receive window the
// ctrl profile pins. It is unexported there, so throughput assertions that need
// to distinguish the two lane profiles restate it here; a single stream on that
// profile cannot exceed ctrlLaneWindowBytes*8/RTT bits per second whatever the
// link and host can do.
const ctrlLaneWindowBytes = 256 << 10

// measureThroughput times one size-byte tunneled download and returns Mbps.
func measureThroughput(t *testing.T, env *testenv.Env, tgt bulkTarget, size int) float64 {
	t.Helper()
	// Warm once so the yamux RTT probe lands before timing.
	_ = streamProxyBulk(t, env, tgt, 1<<20)
	start := time.Now()
	n := streamProxyBulk(t, env, tgt, size)
	elapsed := time.Since(start)
	if n != int64(size) {
		t.Fatalf("throughput sample got %d bytes, want %d", n, size)
	}
	return float64(size*8) / 1e6 / elapsed.Seconds()
}

// pingMS issues one real control-plane RPC (fs.list on the temp dir) through
// the hub and returns its round-trip latency in milliseconds. The RPC rides the
// ctrl stream, so a dedicated ctrl lane keeps it fast while the data lane is
// saturated; on a single lane it queues behind the data flood.
func pingMS(t *testing.T, env *testenv.Env, uuid string) float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := env.Hub().CallNode(ctx, uuid, proto.RPCFSList, map[string]string{"path": os.TempDir()}); err != nil {
		t.Fatalf("ctrl rpc: %v", err)
	}
	return float64(time.Since(start).Microseconds()) / 1000
}
