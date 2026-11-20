package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func TestDatabaseRuntimePolicyRequiresOwner(t *testing.T) {
	snapshot := identity.TransportPolicy{ClusterID: strings.Repeat("a", 32), CAFingerprint: strings.Repeat("b", 64), SourceRevision: 1, AllowedKeys: []string{strings.Repeat("c", 64)}}
	for _, db := range []*Database{nil, {}} {
		if _, err := db.TransportPolicySnapshot(); err == nil {
			t.Fatal("unowned transport returned active snapshot")
		}
		if _, err := db.ApplyTransportPolicySnapshot(snapshot); err == nil {
			t.Fatal("unowned transport accepted runtime update")
		}
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "policy.json")
	if _, err := identity.ApplyTransportPolicySnapshot(path, snapshot); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.OpenRuntimeTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	db := &Database{transportPolicy: owner}
	snapshot.SourceRevision = 2
	snapshot.AllowedKeys = append(snapshot.AllowedKeys, strings.Repeat("d", 64))
	result, err := db.ApplyTransportPolicySnapshot(snapshot)
	if err != nil || result.SourceRevision != 2 || owner.Authorize(strings.Repeat("d", 64)) != nil {
		t.Fatalf("database did not delegate additive update: %#v %v", result, err)
	}
	active, err := db.TransportPolicySnapshot()
	if err != nil || active.SourceRevision != 2 || active.Revision != 2 {
		t.Fatalf("database active snapshot %#v %v", active, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransportPolicySnapshot(); err == nil {
		t.Fatal("closed owner returned database snapshot")
	}
	snapshot.SourceRevision = 3
	if _, err := db.ApplyTransportPolicySnapshot(snapshot); err == nil {
		t.Fatal("closed database owner accepted update")
	}
}
