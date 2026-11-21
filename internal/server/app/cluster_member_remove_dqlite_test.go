//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// removalDatabases opens three actual database members over production
// mutually authenticated TLS and waits until all three vote.
func removalDatabases(t *testing.T, ctx context.Context) []*cluster.Database {
	t.Helper()
	now := time.Now().UTC()
	clusterID := strings.Repeat("c", 32)
	ca, caKey, err := identity.NewAuthority(clusterID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(ca, caKey, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	policy := identity.TransportPolicy{ClusterID: clusterID, CAFingerprint: pin, Revision: 1}
	var configs []cluster.DatabaseConfig
	var certificates, keys [][]byte
	var reserved []net.Listener
	for range 3 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		reserved = append(reserved, listener)
		address := listener.Addr().String()
		key, err := identity.NewMemberKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, err := identity.NewMemberCSR(key)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, err := identity.CSRKeyFingerprint(csr)
		if err != nil {
			t.Fatal(err)
		}
		certificate, err := authority.IssueMember(csr, fingerprint, address, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		dataDir := t.TempDir()
		if err := os.Chmod(dataDir, 0700); err != nil {
			t.Fatal(err)
		}
		configs = append(configs, cluster.DatabaseConfig{Dir: filepath.Join(dataDir, "database"), Address: address, Name: "gpuworks.db"})
		certificates, keys = append(certificates, certificate), append(keys, key)
		policy.AllowedKeys = append(policy.AllowedKeys, fingerprint)
	}
	slices.Sort(policy.AllowedKeys)
	var databases []*cluster.Database
	for i, cfg := range configs {
		policyPath := filepath.Join(filepath.Dir(cfg.Dir), "transport.json")
		if err := identity.PublishTransportPolicy(policyPath, 0, policy); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			cfg.Seeds = []string{configs[0].Address}
		}
		if err := reserved[i].Close(); err != nil {
			t.Fatal(err)
		}
		db, err := cluster.OpenAuthorizedDatabase(ctx, cfg, policyPath, trust, certificates[i], keys[i])
		if err != nil {
			t.Fatal(err)
		}
		databases = append(databases, db)
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	removalWait(t, ctx, "three voters", func() bool {
		members, err := databases[0].Members(ctx)
		return err == nil && len(members) == 3 && !slices.ContainsFunc(members, func(m cluster.Member) bool { return m.Role != "voter" })
	})
	return databases
}

func removalWait(t *testing.T, ctx context.Context, what string, done func() bool) {
	t.Helper()
	for !done() {
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v", what, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// removalMemberListens is the observation these tests supply: a member
// answers when its database port accepts a connection.
func removalMemberListens(ctx context.Context, member cluster.Member) bool {
	dialCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", member.Address)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func TestRemoveClusterMemberKeepsThreeMembersAndAnAnsweringMajority(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	databases := removalDatabases(t, ctx)
	leader := databases[0]
	membership := func() []cluster.Member {
		t.Helper()
		members, err := leader.Members(ctx)
		if err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(members, func(a, b cluster.Member) int { return int(a.ID) - int(b.ID) })
		return members
	}
	before := membership()
	unchanged := func(step string) {
		t.Helper()
		if after := membership(); !slices.Equal(after, before) {
			t.Fatalf("%s changed the membership: %v -> %v", step, before, after)
		}
	}

	// Three healthy voters: no member may leave, voter or not, unless forced.
	for _, db := range databases[1:] {
		if err := RemoveClusterMember(ctx, leader, db.ID, removalMemberListens, false); !errors.Is(err, ErrRemovalLeavesTooFewMembers) {
			t.Fatalf("removal from three members = %v", err)
		}
	}
	if err := ReplaceClusterMember(ctx, leader, databases[1].ID, databases[2].ID, removalMemberListens, false); !errors.Is(err, ErrRemovalLeavesTooFewMembers) {
		t.Fatalf("replacement inside three members = %v", err)
	}
	if err := RemoveClusterMember(ctx, leader, databases[1].ID, nil, true); err == nil {
		t.Fatal("removal without an observation was accepted")
	}
	unchanged("a refused removal")

	// One member stops. The leader still lists it as a voter.
	if err := databases[2].Close(); err != nil {
		t.Fatal(err)
	}
	removalWait(t, ctx, "stopped member's port", func() bool { return !removalMemberListens(ctx, cluster.Member{Address: databases[2].Address}) })
	unchanged("stopping a member")
	// Forcing the other answering voter out would leave one answering voter
	// of two: no leader, and nothing to undo it with.
	if err := RemoveClusterMember(ctx, leader, databases[1].ID, removalMemberListens, true); !errors.Is(err, ErrRemovalBreaksQuorum) {
		t.Fatalf("forced removal of an answering voter beside a stopped one = %v", err)
	}
	if err := RemoveClusterMember(ctx, leader, databases[2].ID, removalMemberListens, false); !errors.Is(err, ErrRemovalLeavesTooFewMembers) {
		t.Fatalf("removal of the stopped member = %v", err)
	}
	unchanged("a refused removal beside a stopped member")

	// Only force removes the stopped member, and two members remain.
	if err := RemoveClusterMember(ctx, leader, databases[2].ID, removalMemberListens, true); err != nil {
		t.Fatal("forced removal of the stopped member", err)
	}
	removalWait(t, ctx, "two members", func() bool {
		members, err := leader.Members(ctx)
		return err == nil && len(members) == 2 && !slices.ContainsFunc(members, func(m cluster.Member) bool { return m.ID == databases[2].ID })
	})
}
