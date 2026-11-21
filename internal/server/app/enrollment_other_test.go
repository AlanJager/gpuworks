//go:build !linux || !dqlite

package app

import (
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestEnrollmentUnsupportedBuildDoesNotCreateState(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitializeEnrollment(t.Context(), Config{DataDir: dir}, "127.0.0.1:9443", "https://127.0.0.1:9443"); err == nil {
		t.Fatal("unsupported enrollment initialized")
	}
	if _, err := PrepareClusterJoin(t.Context(), Config{DataDir: dir}, ClusterJoinTarget{}); err == nil {
		t.Fatal("unsupported build prepared cluster join")
	}
	if _, err := ReceiveClusterMaterials(t.Context(), Config{DataDir: dir}, ClusterJoinTarget{}, "token"); err == nil {
		t.Fatal("unsupported build received cluster materials")
	}
	if err := ReceiveClusterCatalog(t.Context(), Config{DataDir: dir}, ClusterJoinTarget{}); err == nil {
		t.Fatal("unsupported build received catalog packages")
	}
	if _, err := ActivateDatabaseAddress(t.Context(), Config{DataDir: dir}, "127.0.0.1:19000"); err == nil {
		t.Fatal("unsupported build activated database address")
	}
	if err := ReceiveClusterTransportPolicy(t.Context(), Config{DataDir: dir}, ClusterJoinTarget{}); err == nil {
		t.Fatal("unsupported build received transport policy")
	}
	if err := VerifyClusterJoinTransport(t.Context(), Config{DataDir: dir}, ClusterJoinTarget{}); err == nil {
		t.Fatal("unsupported build verified join transport")
	}
	if id, err := PrepareClusterRaftIdentity(t.Context(), Config{DataDir: dir}, ClusterJoinTarget{}); id != 0 || err == nil {
		t.Fatal("unsupported build prepared raft identity")
	}
	if err := ReconcileClusterRaftMarker(t.Context(), Config{DataDir: dir}, ClusterJoinTarget{}); err == nil {
		t.Fatal("unsupported build reconciled a join marker")
	}
	session, err := StartClusterRaft(t.Context(), Config{DataDir: dir}, ClusterJoinTarget{})
	if err == nil || session != nil {
		t.Fatal("unsupported build started raft")
	}
	resumed, err := StartPersistedClusterRaft(t.Context(), Config{DataDir: dir})
	if err == nil || resumed != nil {
		t.Fatal("unsupported build resumed a joined runtime")
	}
	owned, err := OpenPersistedJoinedStore(t.Context(), Config{DataDir: dir}, bus.New())
	if err == nil || owned != nil {
		t.Fatal("unsupported build opened joined runtime storage")
	}
	joined, observed, err := JoinCluster(t.Context(), Config{DataDir: dir}, ClusterJoinTarget{})
	if err == nil || joined != nil || observed.FaultTolerant {
		t.Fatal("unsupported build joined a cluster")
	}
	if _, _, _, err := ConfirmClusterCatchUp(t.Context(), nil); err == nil {
		t.Fatal("unsupported build confirmed catch-up")
	}
	if err := RemoveClusterMember(t.Context(), nil, 1, nil, false); err == nil {
		t.Fatal("unsupported build removed a member")
	}
	if err := ReplaceClusterMember(t.Context(), nil, 1, 2, nil, false); err == nil {
		t.Fatal("unsupported build replaced a member")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unsupported initialization wrote files: %v %v", entries, err)
	}
}
