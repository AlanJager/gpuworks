//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	_ "github.com/glebarez/go-sqlite"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func imageRows(t *testing.T, ctx context.Context, name string, image []byte) int64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, image, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("image integrity = %q, %v", integrity, err)
	}
	var rows int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM image_fixture").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// The image a member returns holds whole committed writes only, also while a
// writer keeps committing, and the writer is not held while it is read.
func TestReadDatabaseImageUnderWrites(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	trust, policy, fixtures := authorizedDatabaseFixtures(t, 3)
	dbs := make([]*Database, 3)
	for i := range fixtures {
		if i > 0 {
			fixtures[i].config.Seeds = []string{fixtures[0].config.Address}
		}
		if err := identity.PublishTransportPolicy(fixtures[i].policyPath, 0, policy); err != nil {
			t.Fatal(err)
		}
		dbs[i] = openAuthorizedFixture(t, ctx, trust, fixtures[i])
	}
	conversionWait(t, ctx, "three voters", func() bool {
		members, err := dbs[0].Members(ctx)
		return err == nil && len(members) == 3 && members[0].Role == "voter" && members[1].Role == "voter" && members[2].Role == "voter"
	})
	leader := dbs[0]
	if _, err := leader.SQL.ExecContext(ctx, "CREATE TABLE image_fixture (id INTEGER PRIMARY KEY, pair INTEGER NOT NULL, body TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 4000)
	// Every transaction inserts two rows, so an image that cut a write in
	// half would hold an odd number of them.
	write := func(pair int64) error {
		tx, err := leader.SQL.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for range 2 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO image_fixture(pair, body) VALUES (?, ?)", pair, body); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		return tx.Commit()
	}
	for pair := range int64(200) {
		if err := write(pair); err != nil {
			t.Fatal(err)
		}
	}
	// A write is committed once a majority has it; the third member applies
	// it a moment later. The bounds below take the 200 transactions written
	// so far as every member's floor, so wait until every member holds them
	// before the concurrent writer starts.
	conversionWait(t, ctx, "every member holding the first 200 transactions", func() bool {
		for i, member := range dbs {
			image, err := ReadDatabaseImage(ctx, member.Address, fixtures[i].config.Name, materialQuorumTLS(t, trust, policy, fixtures[i]))
			if err != nil || imageRows(t, ctx, fixtures[i].config.Name, image) < 400 {
				return false
			}
		}
		return true
	})
	var committed atomic.Int64
	committed.Store(200)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for pair := int64(200); ; pair++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := write(pair); err != nil {
				done <- err
				return
			}
			committed.Add(1)
		}
	}()
	for i, member := range dbs {
		dial := materialQuorumTLS(t, trust, policy, fixtures[i])
		before := committed.Load()
		image, err := ReadDatabaseImage(ctx, member.Address, fixtures[i].config.Name, dial)
		after := committed.Load()
		if err != nil {
			t.Fatalf("member %d: %v", i, err)
		}
		rows := imageRows(t, ctx, fixtures[i].config.Name, image)
		if rows%2 != 0 {
			t.Fatalf("member %d image holds %d rows: half a transaction", i, rows)
		}
		// The leader's image holds everything committed before the read began.
		// A follower may trail by what it has not applied yet.
		if member == leader && (rows < 2*before || rows > 2*(after+1)) {
			t.Fatalf("leader image holds %d rows; %d to %d transactions were committed around the read", rows, before, after)
		}
		if rows < 400 || rows > 2*(after+1) {
			t.Fatalf("member %d image holds %d rows, outside what was ever committed", i, rows)
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("the writer failed while images were read: %v", err)
	}
	if committed.Load() == 200 {
		t.Fatal("the writer committed nothing while images were read; the test did not exercise concurrent writes")
	}
}

func TestReadDatabaseImageRefusals(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	trust, policy, fixtures := authorizedDatabaseFixtures(t, 1)
	member := fixtures[0]
	if err := identity.PublishTransportPolicy(member.policyPath, 0, policy); err != nil {
		t.Fatal(err)
	}
	db := openAuthorizedFixture(t, ctx, trust, member)
	if _, err := db.SQL.ExecContext(ctx, "CREATE TABLE image_fixture (id INTEGER PRIMARY KEY, pair INTEGER, body TEXT)"); err != nil {
		t.Fatal(err)
	}
	dial := materialQuorumTLS(t, trust, policy, member)
	if _, err := ReadDatabaseImage(ctx, db.Address, member.config.Name, nil); err == nil {
		t.Fatal("an image was read without an authenticated dialer")
	}
	if image, err := ReadDatabaseImage(ctx, db.Address, "absent.db", dial); err == nil || image != nil {
		t.Fatalf("a database that does not exist returned %d bytes, %v", len(image), err)
	}
	// Only the named member is dialed, whatever the client library asks for.
	var targets []string
	recording := func(ctx context.Context, target string) (net.Conn, error) {
		targets = append(targets, target)
		return dial(ctx, target)
	}
	if _, err := ReadDatabaseImage(ctx, db.Address, member.config.Name, recording); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target != db.Address {
			t.Fatalf("an image read dialed %s, not the named member %s", target, db.Address)
		}
	}
	if len(targets) == 0 {
		t.Fatal("the supplied dialer was not used")
	}
	// A key of another authority is refused by the member's transport.
	foreignTrust, foreignPolicy, foreign := authorizedDatabaseFixtures(t, 1)
	if image, err := ReadDatabaseImage(ctx, db.Address, member.config.Name, materialQuorumTLS(t, foreignTrust, foreignPolicy, foreign[0])); err == nil || image != nil {
		t.Fatalf("a key outside the cluster read %d bytes, %v", len(image), err)
	}
	for name, files := range map[string][]client.File{
		"no files":            nil,
		"frames beside image": {{Name: "a.db", Data: append([]byte("SQLite format 3\x00"), make([]byte, 100)...)}, {Name: "a.db-wal", Data: []byte("frame")}},
		"not SQLite":          {{Name: "a.db", Data: make([]byte, 200)}, {Name: "a.db-wal"}},
		"another name":        {{Name: "b.db", Data: append([]byte("SQLite format 3\x00"), make([]byte, 100)...)}, {Name: "a.db-wal"}},
	} {
		if image, err := databaseImage("a.db", files); err == nil || image != nil {
			t.Errorf("%s: accepted as a database image", name)
		}
	}
}
