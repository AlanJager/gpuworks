package app

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testNetBenchToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNetBenchProbeMeasuresPeerReceivedBytes(t *testing.T) {
	node := &NodeRuntime{netBench: newNetBenchManager()}
	startedAny, err := node.rpcNetBenchStart(mustJSON(t, netBenchStartParams{Port: 0, Token: testNetBenchToken, TTLSeconds: 5}))
	if err != nil {
		t.Fatal(err)
	}
	started := startedAny.(netBenchStartResult)
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(started.Port))
	resultAny, err := node.rpcNetProbe(context.Background(), mustJSON(t, netProbeParams{
		Target: target, Token: testNetBenchToken, Bandwidth: true, Seconds: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	result := resultAny.(netProbeResult)
	if !result.Reachable || !result.BandwidthTested || result.Gbps <= 0 || result.BytesSent <= 0 || result.BytesReceived <= 0 || result.BytesReceived > result.BytesSent {
		t.Fatalf("result = %+v", result)
	}
	if _, err := node.rpcNetBenchStop(mustJSON(t, netBenchStopParams{Token: testNetBenchToken})); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.DialTimeout("tcp", target, 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("listener remained reachable after stop")
	}
}

func TestNetBenchRejectsWrongToken(t *testing.T) {
	node := &NodeRuntime{netBench: newNetBenchManager()}
	startedAny, err := node.rpcNetBenchStart(mustJSON(t, netBenchStartParams{Port: 0, Token: testNetBenchToken, TTLSeconds: 5}))
	if err != nil {
		t.Fatal(err)
	}
	defer node.netBench.stop(testNetBenchToken)
	started := startedAny.(netBenchStartResult)
	wrong := strings.Repeat("a", 64)
	resultAny, err := node.rpcNetProbe(context.Background(), mustJSON(t, netProbeParams{
		Target: net.JoinHostPort("127.0.0.1", strconv.Itoa(started.Port)), Token: wrong,
	}))
	if err != nil {
		t.Fatal(err)
	}
	result := resultAny.(netProbeResult)
	if result.Reachable || !strings.Contains(result.Error, "rejected authentication") {
		t.Fatalf("result = %+v", result)
	}
}

func TestNetBenchListenerTTLClosesPort(t *testing.T) {
	manager := newNetBenchManager()
	port, err := manager.start(0, testNetBenchToken, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	time.Sleep(80 * time.Millisecond)
	if conn, err := net.DialTimeout("tcp", target, 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("listener remained reachable after TTL")
	}
}

func TestNetProbeRejectsInvalidInputs(t *testing.T) {
	for _, params := range []netProbeParams{
		{Target: "127.0.0.1", Token: testNetBenchToken},
		{Target: "127.0.0.1:29500", Token: "short"},
		{Target: "127.0.0.1:29500", Token: testNetBenchToken, Seconds: 11},
	} {
		if err := validateNetProbeParams(params); err == nil {
			t.Fatalf("params accepted: %+v", params)
		}
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNetBenchProbeBindsTheSourceIP(t *testing.T) {
	node := &NodeRuntime{netBench: newNetBenchManager()}
	startedAny, err := node.rpcNetBenchStart(mustJSON(t, netBenchStartParams{Port: 0, Token: testNetBenchToken, TTLSeconds: 5}))
	if err != nil {
		t.Fatal(err)
	}
	defer node.netBench.stop(testNetBenchToken)
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(startedAny.(netBenchStartResult).Port))
	probe := func(source string) (netProbeResult, error) {
		res, err := node.rpcNetProbe(context.Background(), mustJSON(t, netProbeParams{Target: target, Token: testNetBenchToken, SourceIP: source}))
		if err != nil {
			return netProbeResult{}, err
		}
		return res.(netProbeResult), nil
	}
	if res, err := probe("127.0.0.1"); err != nil || !res.Reachable {
		t.Fatalf("local source = %+v, %v", res, err)
	}
	// An address this host does not own cannot be bound: the probe reports it
	// instead of silently leaving on another interface.
	if res, err := probe("192.0.2.1"); err != nil || res.Reachable || !strings.Contains(res.Error, "assign requested address") {
		t.Fatalf("foreign source = %+v, %v", res, err)
	}
	if _, err := probe("not-an-ip"); err == nil || !strings.Contains(err.Error(), "source_ip") {
		t.Fatalf("invalid source err = %v", err)
	}
}
