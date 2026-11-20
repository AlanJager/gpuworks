//go:build linux && dqlite

package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMaterialReplicationCadenceKeepsMembershipAndRepairUrgent(t *testing.T) {
	now := time.Unix(1700000000, 0)
	interval := time.Minute
	for _, tc := range []struct {
		name        string
		lastFull    time.Time
		lastProbe   time.Time
		sameMembers bool
		wantFull    bool
		wantProbe   bool
	}{
		{name: "first run", wantFull: true},
		{name: "new member", lastFull: now.Add(-interval), lastProbe: now.Add(-interval), wantFull: true},
		{name: "stable idle", lastFull: now.Add(-interval), lastProbe: now.Add(-interval), sameMembers: true},
		{name: "fresh probe due", lastFull: now.Add(-3 * interval), lastProbe: now.Add(-2 * interval), sameMembers: true, wantProbe: true},
		{name: "full scan due", lastFull: now.Add(-5 * interval), lastProbe: now.Add(-2 * interval), sameMembers: true, wantFull: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full, probe := materialReplicationDue(now, interval, tc.lastFull, tc.lastProbe, tc.sameMembers)
			if full != tc.wantFull || probe != tc.wantProbe {
				t.Fatalf("full=%v probe=%v, want full=%v probe=%v", full, probe, tc.wantFull, tc.wantProbe)
			}
		})
	}
}

func TestObservedMembersOnlyShrinkKeepsKnownPublishers(t *testing.T) {
	member := func(id uint64) store.ClusterTransportMember {
		return store.ClusterTransportMember{ID: id, Address: "172.20.9.1:9001", Role: "voter"}
	}
	previous := []store.ClusterTransportMember{member(1), member(2), member(3)}
	if !observedMembersOnlyShrink(previous, []store.ClusterTransportMember{member(1), member(3)}) {
		t.Fatal("a lost voter was treated as a new publisher")
	}
	if observedMembersOnlyShrink(previous, previous) {
		t.Fatal("an unchanged set was treated as a shrink")
	}
	if observedMembersOnlyShrink(previous, []store.ClusterTransportMember{member(1), member(4)}) {
		t.Fatal("a replaced voter was treated as only a loss")
	}
	if observedMembersOnlyShrink(previous, nil) || observedMembersOnlyShrink(nil, previous) {
		t.Fatal("an empty observation kept the previous admission")
	}
	grown := append([]store.ClusterTransportMember{}, previous...)
	grown = append(grown, member(4))
	if observedMembersOnlyShrink(previous, grown) {
		t.Fatal("a new voter was treated as only a loss")
	}
}

func TestMaterialInstallationRunsForChangedOrRepairedBytes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		force       bool
		installed   string
		current     string
		transferred bool
		want        bool
	}{
		{name: "first install", force: true, current: "new", want: true},
		{name: "new manifest", installed: "old", current: "new", want: true},
		{name: "repaired receiver", installed: "same", current: "same", transferred: true, want: true},
		{name: "stable unchanged", installed: "same", current: "same"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldMaterializeManifest(tc.force, tc.installed, tc.current, tc.transferred); got != tc.want {
				t.Fatalf("install=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestMaterialInstallationCadence(t *testing.T) {
	now := time.Unix(1700000000, 0)
	interval := time.Minute
	if !materialInstallationDue(now, time.Time{}, interval, true) || !materialInstallationDue(now, now.Add(-interval), interval, false) {
		t.Fatal("first run or membership change did not trigger installation")
	}
	if materialInstallationDue(now, now.Add(-14*interval), interval, true) {
		t.Fatal("unchanged manifest was reinstalled before the longer cadence")
	}
	if !materialInstallationDue(now, now.Add(-15*interval), interval, true) {
		t.Fatal("unchanged manifest was never rechecked")
	}
}

func TestLocalMaterialInventoryPersistsContentBeforeIndexing(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "shared.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	body := []byte("durable local source bytes")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, body, 0600); err != nil {
		t.Fatal(err)
	}
	memberID := uint64(1<<63 + 17)
	inventory := MaterialInventory{Files: []MaterialFile{
		{Class: MaterialMasterSecret, Path: source, SHA256: digest},
		{Class: MaterialMemberIdentity, Path: source, SHA256: digest},
	}}
	paths, err := persistLocalMaterialInventory(t.Context(), root, st, memberID, inventory)
	if err != nil {
		t.Fatal(err)
	}
	path := paths[digest]
	if path != filepath.Join(root, "material-replicas", digest[:2], digest) {
		t.Fatalf("unexpected canonical path %q", path)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if received, err := os.ReadFile(path); err != nil || !bytes.Equal(received, body) {
		t.Fatalf("canonical bytes depend on original source: %v", err)
	}
	var row store.MaterialReplica
	if err := st.DB().Where("digest = ? AND member_id = ?", digest, int64(memberID)).First(&row).Error; err != nil || row.Path != path {
		t.Fatalf("local durable reference is missing: %+v %v", row, err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := persistLocalMaterialInventory(t.Context(), root, st, memberID, MaterialInventory{Files: []MaterialFile{{Class: MaterialMasterSecret, Path: path, SHA256: digest}}}); err == nil {
		t.Fatal("corrupted canonical file was accepted")
	}
}

func TestWorkerLocalMaterialIndexRefusalReturnsNoReference(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "shared.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := os.Mkdir(filepath.Join(root, "material-replicas"), 0700); err != nil {
		t.Fatal(err)
	}
	serving, err := st.ReadMaintenancePhase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionMaintenancePhase(t.Context(), serving, store.MaintenanceFreezing); err != nil {
		t.Fatal(err)
	}
	memberID := uint64(7)
	first := []byte("worker manifest bytes")
	firstDigest := materialTestDigest(first)
	if path, err := persistLocalMaterialBytes(t.Context(), root, st, memberID, firstDigest, first); !errors.Is(err, store.ErrMaintenanceNotServing) || path != "" {
		t.Fatalf("frozen worker returned a manifest reference: path=%q err=%v", path, err)
	}
	second := []byte("worker inventory bytes")
	secondDigest := materialTestDigest(second)
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, second, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := MaterialInventory{Files: []MaterialFile{{Class: MaterialModelLogo, Path: source, SHA256: secondDigest}}}
	if paths, err := persistLocalMaterialInventory(t.Context(), root, st, memberID, inventory); !errors.Is(err, store.ErrMaintenanceNotServing) || paths != nil {
		t.Fatalf("frozen worker returned inventory references: paths=%v err=%v", paths, err)
	}
	for digest, body := range map[string][]byte{firstDigest: first, secondDigest: second} {
		var indexed int64
		if err := st.DB().Model(&store.MaterialReplica{}).Where("digest = ? AND member_id = ?", digest, int64(memberID)).Count(&indexed).Error; err != nil || indexed != 0 {
			t.Fatalf("frozen worker indexed %s: count=%d err=%v", digest, indexed, err)
		}
		path := filepath.Join(root, "material-replicas", digest[:2], digest)
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, body) {
			t.Fatalf("frozen worker changed or removed unindexed bytes %s: %q %v", digest, got, err)
		}
	}
}
