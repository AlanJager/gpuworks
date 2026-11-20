//go:build linux && dqlite

package cluster

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

type authorizedMemberFixture struct {
	config                  DatabaseConfig
	certificate, key        []byte
	fingerprint, policyPath string
}

func authorizedDatabaseFixtures(t *testing.T, count int) (*identity.Trust, identity.TransportPolicy, []authorizedMemberFixture) {
	t.Helper()
	now := time.Now().UTC()
	clusterID := "c4178b0a97cb456cb65709a4d4126d56"
	ca, key, err := identity.NewAuthority(clusterID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(ca, key, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	policy := identity.TransportPolicy{ClusterID: clusterID, CAFingerprint: pin, Revision: 1}
	members := make([]authorizedMemberFixture, count)
	for i := range members {
		member := &members[i]
		member.config = databaseTLSConfig(t, nil, nil)
		member.config.Dir = filepath.Join(member.config.Dir, "database")
		member.key, err = identity.NewMemberKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, err := identity.NewMemberCSR(member.key)
		if err != nil {
			t.Fatal(err)
		}
		member.fingerprint, err = identity.CSRKeyFingerprint(csr)
		if err != nil {
			t.Fatal(err)
		}
		member.certificate, err = authority.IssueMember(csr, member.fingerprint, member.config.Address, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		member.policyPath = filepath.Join(dir, "transport.json")
		policy.AllowedKeys = append(policy.AllowedKeys, member.fingerprint)
	}
	sort.Strings(policy.AllowedKeys)
	return trust, policy, members
}

func openAuthorizedFixture(t *testing.T, ctx context.Context, trust *identity.Trust, member authorizedMemberFixture) *Database {
	t.Helper()
	db, err := OpenAuthorizedDatabase(ctx, member.config, member.policyPath, trust, member.certificate, member.key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func TestAuthorizedDatabasePersistsAndReopens(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	trust, policy, members := authorizedDatabaseFixtures(t, 1)
	member := members[0]
	if err := identity.PublishTransportPolicy(member.policyPath, 0, policy); err != nil {
		t.Fatal(err)
	}
	db := openAuthorizedFixture(t, ctx, trust, member)
	if _, err := db.Scheduling.ExecContext(ctx, "CREATE TABLE authorized_fixture (id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.ExecContext(ctx, "INSERT INTO authorized_fixture VALUES (1,'normal')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Scheduling.ExecContext(ctx, "INSERT INTO authorized_fixture VALUES (2,'scheduling')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openAuthorizedFixture(t, ctx, trust, member)
	var count int
	if err := db.SQL.QueryRowContext(ctx, "SELECT count(*) FROM authorized_fixture WHERE (id=1 AND value='normal') OR (id=2 AND value='scheduling')").Scan(&count); err != nil || count != 2 {
		t.Fatalf("persisted writes: count=%d err=%v", count, err)
	}
	if _, err := db.Scheduling.ExecContext(ctx, "INSERT INTO authorized_fixture VALUES (3,'after restart')"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizedDatabaseRejectsBeforeCreatingState(t *testing.T) {
	requireDatabaseHelper(t)
	for _, name := range []string{"missing", "wrong-cluster", "wrong-ca", "revoked-local", "tls-bypass", "nil-trust"} {
		t.Run(name, func(t *testing.T) {
			trust, policy, members := authorizedDatabaseFixtures(t, 2)
			member := members[0]
			switch name {
			case "wrong-cluster":
				policy.ClusterID = "d4178b0a97cb456cb65709a4d4126d56"
			case "wrong-ca":
				policy.CAFingerprint = strings.Repeat("0", 64)
			case "revoked-local":
				policy.AllowedKeys = []string{members[1].fingerprint}
				policy.RevokedKeys = []string{member.fingerprint}
			case "tls-bypass":
				member.config.DialTLS = &tls.Config{MinVersion: tls.VersionTLS13}
			case "nil-trust":
				trust = nil
			}
			if name != "missing" {
				if err := identity.PublishTransportPolicy(member.policyPath, 0, policy); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			db, err := OpenAuthorizedDatabase(ctx, member.config, member.policyPath, trust, member.certificate, member.key)
			if err == nil {
				_ = db.Close()
				t.Fatal("unauthorized database opened")
			}
			if _, err := os.Stat(member.config.Dir); !os.IsNotExist(err) {
				t.Fatalf("rejection created database state: %v", err)
			}
		})
	}
}

func TestAuthorizedDatabaseQuorumSurvivesAndRejoinsFromPolicy(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	trust, policy, members := authorizedDatabaseFixtures(t, 3)
	for i := range members {
		if i > 0 {
			members[i].config.Seeds = []string{members[0].config.Address}
		}
		if err := identity.PublishTransportPolicy(members[i].policyPath, 0, policy); err != nil {
			t.Fatal(err)
		}
	}
	a := openAuthorizedFixture(t, ctx, trust, members[0])
	if _, err := a.SQL.ExecContext(ctx, "CREATE TABLE authorized_fixture (id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	b := openAuthorizedFixture(t, ctx, trust, members[1])
	c := openAuthorizedFixture(t, ctx, trust, members[2])
	conversionWait(t, ctx, "three policy-authorized voters", func() bool {
		current, err := a.Members(ctx)
		return err == nil && len(current) == 3 && current[0].Role == "voter" && current[1].Role == "voter" && current[2].Role == "voter"
	})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	conversionWait(t, ctx, "surviving majority commits", func() bool {
		_, err := b.SQL.ExecContext(ctx, "INSERT INTO authorized_fixture VALUES (1,'survived') ON CONFLICT(id) DO NOTHING")
		return err == nil
	})
	var value string
	if err := c.SQL.QueryRowContext(ctx, "SELECT value FROM authorized_fixture WHERE id=1").Scan(&value); err != nil || value != "survived" {
		t.Fatalf("survivor read: %q %v", value, err)
	}
	a = openAuthorizedFixture(t, ctx, trust, members[0])
	if err := a.SQL.QueryRowContext(ctx, "SELECT value FROM authorized_fixture WHERE id=1").Scan(&value); err != nil || value != "survived" {
		t.Fatalf("persisted-policy rejoin: %q %v", value, err)
	}
}

func TestAuthorizedDatabaseOwnsPolicyUntilClosed(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	trust, policy, members := authorizedDatabaseFixtures(t, 1)
	member := members[0]
	if err := identity.PublishTransportPolicy(member.policyPath, 0, policy); err != nil {
		t.Fatal(err)
	}
	db := openAuthorizedFixture(t, ctx, trust, member)
	next := policy
	next.Revision++
	if err := identity.PublishTransportPolicy(member.policyPath, policy.Revision, next); err == nil {
		t.Fatal("published new authorization while transport still live")
	}
	loaded, err := identity.LoadTransportPolicy(member.policyPath)
	if err != nil || loaded.Revision != policy.Revision {
		t.Fatalf("busy publication changed policy: revision=%d err=%v", loaded.Revision, err)
	}
	duplicate, err := OpenAuthorizedDatabase(ctx, member.config, member.policyPath, trust, member.certificate, member.key)
	if err == nil {
		_ = duplicate.Close()
		t.Fatal("second transport acquired policy held by first transport")
	}
	// A rejected second owner must not release the first owner's lease.
	if err := identity.PublishTransportPolicy(member.policyPath, policy.Revision, next); err == nil {
		t.Fatal("rejected duplicate owner released live policy lease")
	}
	if _, err := db.SQL.ExecContext(ctx, "CREATE TABLE lease_fixture (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("original transport stopped after rejected duplicate: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.PingContext(ctx); err == nil {
		t.Fatal("normal pool still usable after Close")
	}
	if err := db.Scheduling.PingContext(ctx); err == nil {
		t.Fatal("scheduling pool still usable after Close")
	}
	if err := identity.PublishTransportPolicy(member.policyPath, policy.Revision, next); err != nil {
		t.Fatalf("closed transport retained policy lease: %v", err)
	}
	reopened := openAuthorizedFixture(t, ctx, trust, member)
	if _, err := reopened.Scheduling.ExecContext(ctx, "INSERT INTO lease_fixture VALUES (1)"); err != nil {
		t.Fatalf("revision 2 restart: %v", err)
	}
	loaded, err = identity.LoadTransportPolicy(member.policyPath)
	if err != nil || loaded.Revision != next.Revision {
		t.Fatalf("reopened policy revision=%d err=%v", loaded.Revision, err)
	}
}

func TestAuthorizedDatabaseFailedOpenReleasesPolicy(t *testing.T) {
	requireDatabaseHelper(t)
	trust, policy, members := authorizedDatabaseFixtures(t, 1)
	member := members[0]
	if err := identity.PublishTransportPolicy(member.policyPath, 0, policy); err != nil {
		t.Fatal(err)
	}
	// Transport policy and certificates pass validation; database validation fails
	// after policy acquisition, exercising the failed-open ownership path.
	member.config.Name = "../invalid.db"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := OpenAuthorizedDatabase(ctx, member.config, member.policyPath, trust, member.certificate, member.key)
	if err == nil {
		_ = db.Close()
		t.Fatal("invalid database name accepted")
	}
	next := policy
	next.Revision++
	if err := identity.PublishTransportPolicy(member.policyPath, policy.Revision, next); err != nil {
		t.Fatalf("failed open retained policy lease: %v", err)
	}
	if _, err := os.Stat(member.config.Dir); !os.IsNotExist(err) {
		t.Fatalf("failed validation created database state: %v", err)
	}
}
