package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestProbeTargetUsesHostOnlyAddress(t *testing.T) {
	for input, want := range map[string]string{
		"10.0.0.2":     "10.0.0.2:29500",
		"2001:db8::10": "[2001:db8::10]:29500",
	} {
		got, err := probeTarget(input, 29500)
		if err != nil || got != want {
			t.Fatalf("probeTarget(%q)=%q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := probeTarget("10.0.0.2:19090", 29500); err == nil {
		t.Fatal("address with port was accepted")
	}
}

func TestProbeTargetRequiresPort(t *testing.T) {
	if _, err := probeTarget("10.0.0.2", 0); err == nil {
		t.Fatal("expected missing port error")
	}
}

func TestDistributedPreflightOrchestratesAuthenticatedDirectedPairs(t *testing.T) {
	st, err := store.Open("file:preflight_orchestration_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[string]string{"node-a": "10.0.0.1", "node-b": "10.0.0.2"}
	for uuid, host := range hosts {
		node, err := st.RegisterNode(uuid, store.HashToken("token"), uuid)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{Network: testManagement(host)}); err != nil {
			t.Fatal(err)
		}
	}

	tokens := map[string]string{}
	starts, probes, stops := 0, 0, 0
	call := func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
		raw, _ := json.Marshal(params)
		switch method {
		case proto.RPCNetBenchStart:
			var p netBenchStartParamsForTest
			_ = json.Unmarshal(raw, &p)
			if len(p.Token) != 64 || p.Port != 29500 || p.TTLSeconds < 60 {
				t.Fatalf("start params = %+v", p)
			}
			tokens[uuid] = p.Token
			starts++
			return json.RawMessage(`{"port":29500}`), nil
		case proto.RPCNetProbe:
			var p struct {
				Target    string `json:"target"`
				Token     string `json:"token"`
				Bandwidth bool   `json:"bandwidth"`
				Seconds   int    `json:"seconds"`
			}
			_ = json.Unmarshal(raw, &p)
			targetUUID := "node-a"
			if strings.HasPrefix(p.Target, hosts["node-b"]+":") {
				targetUUID = "node-b"
			}
			if p.Token != tokens[targetUUID] || !p.Bandwidth || p.Seconds != 1 {
				t.Fatalf("probe params = %+v target=%s", p, targetUUID)
			}
			probes++
			return json.RawMessage(`{"reachable":true,"rtt_ms_p50":0.4,"rtt_ms_p99":0.8,"gbps":12.5,"bandwidth_tested":true,"bytes_sent":1000,"bytes_received":1000,"duration_seconds":1}`), nil
		case proto.RPCNetBenchStop:
			var p struct {
				Token string `json:"token"`
			}
			_ = json.Unmarshal(raw, &p)
			if p.Token != tokens[uuid] {
				t.Fatalf("stop token mismatch for %s", uuid)
			}
			stops++
			return json.RawMessage(`{"stopped":true}`), nil
		default:
			t.Fatalf("unexpected method %q", method)
			return nil, nil
		}
	}
	h := apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }, CallNode: call}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/preflight", bytes.NewBufferString(`{"node_uuids":["node-a","node-b"],"port":29500,"master_addr":"10.0.0.9","bandwidth":true,"seconds":1}`))
	rec := httptest.NewRecorder()
	h.distributedPreflight(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result distributedPreflightResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || len(result.Pairs) != 2 || starts != 2 || probes != 2 || stops != 2 {
		t.Fatalf("result=%+v starts=%d probes=%d stops=%d", result, starts, probes, stops)
	}
	for _, pair := range result.Pairs {
		if !pair.Reachable || !pair.BandwidthTested || pair.Gbps != 12.5 || pair.BytesReceived != 1000 {
			t.Fatalf("pair=%+v", pair)
		}
		if pair.TargetUUID == "node-a" && !strings.HasPrefix(pair.Target, "10.0.0.9:") {
			t.Fatalf("leader target did not use master_addr: %+v", pair)
		}
	}
}

func TestPreflightPairRequiresRequestedBandwidth(t *testing.T) {
	result := distributedPreflightResponse{OK: true}
	result.addPair(preflightPair{SourceUUID: "a", TargetUUID: "b", Reachable: true, RTTP50MS: 1}, true)
	if result.OK || len(result.Warnings) != 1 || len(result.Pairs) != 1 {
		t.Fatalf("result=%+v", result)
	}

	latencyOnly := distributedPreflightResponse{OK: true}
	latencyOnly.addPair(preflightPair{SourceUUID: "a", TargetUUID: "b", Reachable: true, RTTP50MS: 1}, false)
	if !latencyOnly.OK {
		t.Fatalf("latency-only result=%+v", latencyOnly)
	}
}

func TestDistributedPreflightRejectsDuplicateNodes(t *testing.T) {
	h := apiHandlers{d: Deps{CallNode: func(context.Context, string, string, any) (json.RawMessage, error) { return nil, nil }}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/preflight", bytes.NewBufferString(`{"node_uuids":["same","same"]}`))
	rec := httptest.NewRecorder()
	h.distributedPreflight(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type netBenchStartParamsForTest struct {
	Port       int    `json:"port"`
	Token      string `json:"token"`
	TTLSeconds int    `json:"ttl_seconds"`
}
