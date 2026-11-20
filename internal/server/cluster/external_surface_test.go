package cluster_test

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/filelock"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// The cluster commands and the database process are not built from this
// repository: a separately built server registers the commands through
// cli.Options.Commands and ships the database process beside its executable.
// These tests pin what that code reaches in the engine, by exact signature
// and by the values it compares. Changing one of them changes a contract.

// The commands read a member's quorum and leader over the database port and
// set the storage tier the engine starts database nodes with.
func TestClusterCommandsReachQuorumLeaderAndStorageTier(t *testing.T) {
	var (
		_ func(context.Context, string, func(context.Context, string) (net.Conn, error)) (cluster.QuorumProof, error)            = cluster.ReadQuorumProof
		_ func(context.Context, string, func(context.Context, string) (net.Conn, string, error)) (cluster.Member, string, error) = cluster.ObserveRaftLeader
		_ func(storagetier.Tier)                                                                                                 = cluster.UseStorageTier
	)
	proof := cluster.QuorumProof{Term: 3, Index: 9, ConfigurationIndex: 7, LeaderID: 1, Members: []cluster.Member{{ID: 1, Address: "10.0.0.1:9001", Role: "voter"}}}
	if proof.Members[0].ID != proof.LeaderID || proof.Members[0].Role != "voter" {
		t.Fatalf("proof = %+v", proof)
	}
}

// The database process announces the contract version it speaks and the
// server starts only the one it supports.
func TestDatabaseProcessProtocolVersion(t *testing.T) {
	if cluster.HelperProtocolVersion != 1 {
		t.Fatalf("HelperProtocolVersion = %d; raising it is a contract change with every shipped database process", cluster.HelperProtocolVersion)
	}
}

// The commands and the database process read the identity files the engine
// writes; the commands' tests build the same material.
func TestClusterCommandsAndDatabaseProcessReadIdentity(t *testing.T) {
	var (
		_ func(string) (identity.TransportPolicy, error)                               = identity.LoadTransportPolicy
		_ func(string, uint64, identity.TransportPolicy) error                         = identity.PublishTransportPolicy
		_ func([]byte, string, string, time.Time) (*identity.Trust, error)             = identity.NewTrust
		_ func(string) ([]byte, error)                                                 = identity.LoadMemberKey
		_ func() ([]byte, error)                                                       = identity.NewMemberKey
		_ func([]byte) ([]byte, error)                                                 = identity.NewMemberCSR
		_ func([]byte) (string, error)                                                 = identity.CSRKeyFingerprint
		_ func([]byte) (string, error)                                                 = identity.CertificateFingerprint
		_ func(string, time.Time) ([]byte, []byte, error)                              = identity.NewAuthority
		_ func([]byte, []byte, string, string, time.Time) (*identity.Authority, error) = identity.LoadAuthority
	)
	policy := identity.TransportPolicy{ClusterID: "c", CAFingerprint: "f", Revision: 1, AllowedKeys: []string{"k"}, RevokedKeys: []string{}}
	if policy.ClusterID != "c" || policy.CAFingerprint != "f" || len(policy.AllowedKeys) != 1 {
		t.Fatalf("policy = %+v", policy)
	}
	if _, err := identity.LoadTransportPolicy(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("a missing transport policy loaded")
	}
}

// The commands record a tier by name and the database process is started with
// that tier's network latency.
func TestClusterCommandsAndDatabaseProcessShareStorageTiers(t *testing.T) {
	for name, want := range map[string]storagetier.Tier{"": storagetier.Standard, "standard": storagetier.Standard, "slow-storage": storagetier.SlowStorage} {
		if got, err := storagetier.Parse(name); err != nil || got != want {
			t.Fatalf("Parse(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := storagetier.Parse("glacial"); err == nil {
		t.Fatal("an unknown tier parsed")
	}
	for _, tier := range []storagetier.Tier{storagetier.Standard, storagetier.SlowStorage} {
		if got, ok := storagetier.ForNetworkLatency(tier.NetworkLatency()); !ok || got != tier {
			t.Fatalf("ForNetworkLatency(%s) = %q, %v; want %q", tier.NetworkLatency(), got, ok, tier)
		}
	}
	if _, ok := storagetier.ForNetworkLatency(time.Nanosecond); ok {
		t.Fatal("a latency no tier uses was accepted")
	}
	if !storagetier.SlowStorage.Slower(storagetier.Standard) || storagetier.Standard.Slower(storagetier.SlowStorage) {
		t.Fatal("slow-storage must order after standard")
	}
}

// The installation renders the server.yaml of each member.
func TestClusterCommandsRenderTheServerFile(t *testing.T) {
	raw, err := serverconfig.Render(serverconfig.File{
		Listen:  serverconfig.Listen{Console: []string{"10.0.0.1:8800"}},
		DataDir: "/srv/gpuworks/data",
		Log:     serverconfig.Log{File: "/srv/gpuworks/logs/server.log"},
	})
	if err != nil || len(raw) == 0 {
		t.Fatalf("Render = %q, %v", raw, err)
	}
	if got := serverconfig.Path("/srv/gpuworks"); got != "/srv/gpuworks/server.yaml" {
		t.Fatalf("Path = %q", got)
	}
}

// The installation compares the enrollment protocol version of both ends.
func TestClusterCommandsCarryTheEnrollmentProtocolVersion(t *testing.T) {
	if proto.ClusterProtocolVersion != 1 {
		t.Fatalf("ClusterProtocolVersion = %d", proto.ClusterProtocolVersion)
	}
}

// The database process holds a lock beside its raft directory while it runs.
func TestDatabaseProcessLocksItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft.helper.lock")
	release, err := filelock.Acquire(path)
	if err != nil {
		t.Skipf("file locks are not supported here: %v", err)
	}
	if _, err := filelock.Acquire(path); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("second holder = %v, want ErrBusy", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}
