//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The member that created a cluster answers, in the process that created
// it, the questions every member is asked: the quorum facts, the discovery
// of the members, the upgrade status and its part of a diagnostics
// collection. Before, that process answered each of them with 404 until it
// was restarted. A step that changes the cluster is refused there with a
// reason of its own, and the answer says which kind of process gave it.
func TestTheProcessThatCreatedTheClusterAnswersTheMemberQuestions(t *testing.T) {
	cfg, address := enrollmentNativeConfig(t)
	info, err := InitializeEnrollmentWithTier(t.Context(), cfg, address, "https://"+address, storagetier.SlowStorage)
	if err != nil {
		t.Fatal(err)
	}
	// As an installation does: the database exists, then it is given the
	// member's address and certificate, then the server starts.
	if err := os.Chmod(cfg.DataDir, 0750); err != nil {
		t.Fatal(err)
	}
	created, err := store.OpenRuntime(t.Context(), filepath.Join(cfg.DataDir, "gpuworks.db"), bus.New(), store.ReplicatedBackend())
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	database := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateDatabaseAddress(t.Context(), cfg, database); err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, enrollmentNativeLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	startEnrollmentNativeApp(t, a)
	files := enrollmentNativeFiles(t, cfg)
	cert, key := []byte(files["server.pem"]), []byte(files["server.key"])
	client, err := enrollment.NewClient("https://"+a.EnrollmentAddr()+enrollmentCertificatePath, info.ClusterID, info.CAFingerprint, info.Protocol)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	notFound := func(err error) bool { return err != nil && strings.Contains(err.Error(), "404") }

	facts, err := client.FetchObservedQuorumFacts(ctx, cert, key)
	if err != nil || notFound(err) || facts.ClusterID != info.ClusterID || facts.LeaderID == 0 || len(facts.Members) != 1 {
		t.Fatalf("quorum facts: %+v, %v; want this cluster with its one member leading", facts, err)
	}
	self := facts.Members[0]
	discovery, _, err := client.FetchClusterQuorumDiscovery(ctx, cert, key, []string{self.KeyFingerprint})
	if err != nil || len(discovery.Members) != 1 || discovery.Members[0].ID != self.ID {
		t.Fatalf("quorum discovery: %+v, %v; want the one member", discovery, err)
	}
	status, err := client.ClusterUpgrade(ctx, cert, key, enrollment.ClusterUpgradeStatus)
	if err != nil || !status.FoundingProcess || !status.Leader || status.Refused != "" || status.Error != "" || status.Known == 0 || status.Applied != status.Known {
		t.Fatalf("upgrade status: %+v, %v; want the founding process's answer with the schema it runs", status, err)
	}
	now := time.Now()
	part, err := client.OpenClusterDiagnostics(ctx, cert, key, now.Add(-time.Hour), now)
	if notFound(err) {
		t.Fatalf("diagnostics part: %v; the route is not served", err)
	}
	if err == nil && part.Body != nil {
		_ = part.Body.Close()
	}

	// Every step that changes the cluster is refused here, with the status
	// and the reason; none is applied.
	for _, action := range []string{enrollment.ClusterUpgradeBegin, enrollment.ClusterUpgradeMigrate, enrollment.ClusterUpgradeFinish, enrollment.ClusterUpgradeAbort, enrollment.ClusterUpgradeLead} {
		answer, err := client.ClusterUpgrade(ctx, cert, key, action)
		if err != nil || !answer.FoundingProcess || answer.Error != "" ||
			!strings.Contains(answer.Refused, "still runs the process that created the cluster; restart it once before it takes part in an upgrade") {
			t.Fatalf("%s: %+v, %v; want the founding process's refusal", action, answer, err)
		}
		if answer.Phase != status.Phase || answer.PhaseRevision != status.PhaseRevision || answer.Applied != status.Applied {
			t.Fatalf("%s changed the cluster: phase %q revision %d applied %d, was %q %d %d", action, answer.Phase, answer.PhaseRevision, answer.Applied, status.Phase, status.PhaseRevision, status.Applied)
		}
	}
	if !errors.Is(store.ErrClusterUpgradeFoundingProcess, store.ErrClusterUpgradeRefused) {
		t.Fatal("the founding process's refusal is not an upgrade refusal")
	}

}
