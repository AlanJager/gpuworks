//go:build linux && dqlite

package cluster

import (
	"context"
	"testing"
	"time"
)

func TestReplicatedDatabaseAdmittedMemberCertificates(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	// The policy file is the authorization: three admitted members and one
	// with a certificate from the same authority that was never admitted.
	transport := newTestTransport(t)
	configs := make([]DatabaseConfig, 4)
	for index := range configs {
		cfg := databaseTLSConfig(t, nil, nil)
		if index < 3 {
			transport.admit(t, "127.0.0.1").apply(&cfg)
		} else {
			transport.issue(t, "127.0.0.1").apply(&cfg)
		}
		configs[index] = cfg
	}
	open := func(config DatabaseConfig) *Database {
		t.Logf("opening member %s seeds=%v", config.Address, config.Seeds)
		db, err := OpenDatabase(ctx, config)
		if err != nil {
			t.Fatalf("open %s: %v", config.Address, err)
		}
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		})
		return db
	}
	a := open(configs[0])
	if _, err := a.SQL.ExecContext(ctx, "CREATE TABLE identity_fixture (id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	for index := 1; index < len(configs); index++ {
		configs[index].Seeds = []string{a.Address}
	}
	b, c := open(configs[1]), open(configs[2])
	conversionWait(t, ctx, "three individually authenticated voters", func() bool {
		members, err := a.Members(ctx)
		return err == nil && len(members) == 3 && members[0].Role == "voter" && members[1].Role == "voter" && members[2].Role == "voter"
	})
	rejected, stop := context.WithTimeout(ctx, 2*time.Second)
	outsider, err := OpenDatabase(rejected, configs[3])
	stop()
	if err == nil {
		_ = outsider.Close()
		t.Fatal("valid but unauthorized certificate joined")
	}
	members, err := a.Members(ctx)
	if err != nil || len(members) != 3 {
		t.Fatalf("unauthorized member changed topology: %+v %v", members, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	conversionWait(t, ctx, "two authenticated survivors commit", func() bool {
		_, err := b.SQL.ExecContext(ctx, "INSERT INTO identity_fixture VALUES (1,'survived') ON CONFLICT(id) DO NOTHING")
		return err == nil
	})
	var value string
	if err := c.SQL.QueryRowContext(ctx, "SELECT value FROM identity_fixture WHERE id=1").Scan(&value); err != nil || value != "survived" {
		t.Fatalf("committed result lost: %q %v", value, err)
	}
	a = open(configs[0])
	if err := a.SQL.QueryRowContext(ctx, "SELECT value FROM identity_fixture WHERE id=1").Scan(&value); err != nil || value != "survived" {
		t.Fatalf("member restart lost result: %q %v", value, err)
	}
}
