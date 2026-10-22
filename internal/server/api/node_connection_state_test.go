package api

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// One answer about a node's connection, given to every reader. A node the
// platform is restoring over SSH reads as connecting in its own DTO, in the
// list the "connecting" filter returns, and in the dashboard's count — the
// badge saying one thing while the filter says another is the bug this
// pins.
func TestARecoveringNodeReadsAsConnectingEverywhere(t *testing.T) {
	n := newNetworkAPI(t)
	for _, name := range []string{"node-recovering", "node-gave-up"} {
		if _, err := n.store.RegisterNode(name, store.HashToken(name), name); err != nil {
			t.Fatal(err)
		}
		if err := n.store.UpdateNodeStatus(name, proto.NodeStatus{Sys: proto.SysInfo{Hostname: name}}); err != nil {
			t.Fatal(err)
		}
		if err := n.store.SetNodeState(name, store.NodeStateOffline); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.store.SetNodeRecoveryPhase("node-recovering", store.RecoveryPhaseUploading, 1, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := n.store.SetNodeRecoveryPhase("node-gave-up", store.RecoveryPhaseUnreachable, 6, nil, "ssh: connection refused"); err != nil {
		t.Fatal(err)
	}

	var detail dto.Node
	if err := json.Unmarshal(n.memberGet("/api/v1/nodes/node-recovering"), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.State != store.NodeStateConnecting || detail.RecoveryPhase != store.RecoveryPhaseUploading {
		t.Fatalf("node detail = %q phase %q, want connecting with the phase kept beside it", detail.State, detail.RecoveryPhase)
	}

	page := n.nodePage("/api/v1/nodes?page=1&state=connecting")
	if got := nodePageNames(page); len(got) != 1 || got[0] != "node-recovering" {
		t.Fatalf("state=connecting matched %v, want the node being restored", got)
	}
	page = n.nodePage("/api/v1/nodes?page=1&state=offline")
	if got := nodePageNames(page); len(got) != 1 || got[0] != "node-gave-up" {
		t.Fatalf("state=offline matched %v, want the node recovery gave up on", got)
	}

	var summary dto.DashboardSummary
	if err := json.Unmarshal(n.memberGet("/api/v1/dashboard/summary"), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Nodes.Connecting != 1 || summary.Nodes.Offline != 1 || summary.Nodes.Online != 0 {
		t.Fatalf("dashboard counts = %+v, want one connecting and one offline", summary.Nodes)
	}
}
