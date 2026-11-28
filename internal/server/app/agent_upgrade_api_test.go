package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type upgradeAdapterTransport struct{}

func (upgradeAdapterTransport) AgentUpgradeConnection(uuid string) (agentupgrade.Connection, bool) {
	return agentupgrade.Connection{SessionID: "old-" + uuid, Version: "old", Ready: true, Caps: []string{proto.CapUpgradePush}}, true
}
func (upgradeAdapterTransport) PushAgentUpgrade(context.Context, string, string, string, []byte) error {
	return errors.New("adapter must record intent without dispatch")
}
func (upgradeAdapterTransport) RequestAgentUpgrade(context.Context, string, string) error {
	return errors.New("adapter must record intent without dispatch")
}

// upgradeAdapterGateway answers that nothing serves on the nodes: the adapter
// test is about the wire, not about draining.
type upgradeAdapterGateway struct{}

func (upgradeAdapterGateway) ServedElsewhere(uint, []string) bool { return false }
func (upgradeAdapterGateway) Avoid(uint, []string) func()         { return func() {} }
func (upgradeAdapterGateway) InstancesInFlight([]string) int      { return 0 }

func TestAgentUpgradeAPIAdapterPreservesScopeAuditAndWire(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, uuid := range []string{"one", "two"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.SetNodeAgentInfo(uuid, "old", ""); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Sys: proto.SysInfo{OS: "linux", Arch: "amd64"}}); err != nil {
			t.Fatal(err)
		}
	}
	service, err := agentupgrade.New(st, agentupgrade.Config{TargetVersion: "target", ArtifactDir: t.TempDir(), Transport: upgradeAdapterTransport{}, Gateway: upgradeAdapterGateway{}, ReadBinary: func(string) ([]byte, error) { return []byte("verified-fixture"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	adapter := agentUpgradeAPI{service: service}
	preview, err := adapter.Preview(ctx, nil)
	if err != nil || len(preview.NodeUUIDs) != 2 || preview.Counts["eligible"] != 2 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	request := dto.AgentUpgradeCreateRequest{NodeUUIDs: preview.NodeUUIDs, ExpectedTargetVersion: preview.TargetVersion, ExpectedManifestFingerprint: preview.ManifestFingerprint, IdempotencyKey: "app-adapter"}
	run, err := adapter.Create(ctx, request, "creator")
	if err != nil || run.RequestedBy != "creator" || run.Counts.Queued != 2 {
		t.Fatalf("created=%+v err=%v", run, err)
	}
	loaded, err := adapter.Get(ctx, run.ID)
	if err != nil || loaded.ID != run.ID || loaded.BlockedByItem == nil {
		t.Fatalf("get=%+v err=%v", loaded, err)
	}
	page, err := adapter.List(ctx, 1, 1)
	if err != nil || len(page.Items) != 1 || page.Total != 1 || page.BlockedByItem == nil {
		t.Fatalf("list=%+v err=%v", page, err)
	}
	items, err := adapter.Items(ctx, run.ID, 1, 1, "queued")
	if err != nil || len(items.Items) != 1 || items.Total != 2 || items.Items[0].CurrentVersion != "old" || items.Items[0].TargetVersion != "target" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	body, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"artifact_sha256", "baseline_session_id", "idempotency_key"} {
		if strings.Contains(string(body), private) {
			t.Fatalf("private field in wire: %s", body)
		}
	}
	stopped, err := adapter.Stop(ctx, run.ID, "stopper")
	if err != nil || stopped.StopRequestedBy != "stopper" || stopped.Counts.Cancelled != 2 {
		t.Fatalf("stop=%+v err=%v", stopped, err)
	}
	continued, err := adapter.ContinuePreview(ctx, run.ID)
	if err != nil || len(continued.NodeUUIDs) != 2 {
		t.Fatalf("continue=%+v err=%v", continued, err)
	}
	single, err := adapter.UpgradeNode(ctx, "one", "single-actor")
	if err != nil || single.RequestedBy != "single-actor" || single.Counts.Total != 1 {
		t.Fatalf("single=%+v err=%v", single, err)
	}
	if _, err := adapter.BeginServerUpgrade(); err == nil {
		t.Fatal("active single upgrade must hold server gate")
	}
	if err := adapter.Release(ctx, single.ID, "one", "operator", "checked"); err == nil {
		t.Fatal("queued intent cannot be released as an uncertain attempt")
	}
	if _, err := adapter.Items(ctx, 999999, 1, 20, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing run: %v", err)
	}
}

func TestUpgradePreviewFlagsOnlyKnownReleaseDowngrades(t *testing.T) {
	p := agentupgrade.Preview{TargetVersion: "v26.11.2"}
	for _, v := range []string{"v26.11.10", "v26.11.1", "b913427a"} {
		p.Nodes = append(p.Nodes, agentupgrade.Candidate{FromVersion: v, Eligibility: "eligible"})
	}
	got := toAgentUpgradePreview(p)
	if got.Nodes[0].VersionChange != "downgrade" || got.Nodes[1].VersionChange != "upgrade" || got.Nodes[2].VersionChange != "different_build" {
		t.Fatal(got.Nodes)
	}
	if !got.Nodes[0].Downgrade || got.Nodes[1].Downgrade || got.Nodes[2].Downgrade {
		t.Fatal(got.Nodes)
	}
}

// The stage the console shows while a node drains is a value of the item
// DTO's existing phase field: the wire keeps its shape and its keys.
func TestAgentUpgradeItemWireCarriesTheDrainingStage(t *testing.T) {
	started := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	deadline := started.Add(10 * time.Minute)
	got := toAgentUpgradeItem(agentupgrade.ItemView{Record: store.AgentUpgradeItem{RunID: 3, NodeUUID: "n1", Phase: "draining", StartedAt: &started, DeadlineAt: &deadline}}, "v26.12.0")
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"run_id":3,"node_uuid":"n1","name":"","from_version":"","target_version":"v26.12.0","current_version":"","os":"","arch":"","transport":"","phase":"draining","started_at":"2026-11-27T09:00:00Z","deadline_at":"2026-11-27T09:10:00Z"}`
	if string(body) != want {
		t.Fatalf("item wire =\n%s\nwant\n%s", body, want)
	}
}
