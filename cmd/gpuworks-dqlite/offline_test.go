//go:build linux && cgo && dqlite

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"github.com/canonical/go-dqlite/v3/driver"
)

const offlineDatabase = "gpuworks.db"

func openSQL(t *testing.T, address string, dial client.DialFunc) *sql.DB {
	t.Helper()
	store := client.NewInmemNodeStore()
	if err := store.Set(context.Background(), []client.NodeInfo{{Address: address}}); err != nil {
		t.Fatal(err)
	}
	options := []driver.Option{}
	if dial != nil {
		options = append(options, driver.WithDialFunc(dial))
	}
	d, err := driver.New(store, options...)
	if err != nil {
		t.Fatal(err)
	}
	connector, err := d.OpenConnector(offlineDatabase)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// stoppedSource serves a single member, writes application rows and stops it,
// leaving the directory an offline operation works on.
func stoppedSource(t *testing.T) (dir, address string, id uint64, schema string) {
	t.Helper()
	dir, address = dataDir(t), freeAddress(t)
	node := startServe(t, "--dir", dir, "--address", address)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openSQL(t, address, nil)
	for _, statement := range []string{
		"CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"CREATE INDEX settings_value ON settings(value)",
		"INSERT INTO settings VALUES('a','1'),('b','2')",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	// The digest the server recomputes: one JSON row per schema object.
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema ORDER BY type, name")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	for rows.Next() {
		var row [4]string
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3]); err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(hash).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	node.stop(t, syscall.SIGTERM)
	return dir, address, node.id, hex.EncodeToString(hash.Sum(nil))
}

func decodeExactly(t *testing.T, text string, into any) {
	t.Helper()
	if !strings.HasSuffix(text, "\n") || strings.Count(text, "\n") != 1 {
		t.Fatalf("output %q is not one line", text)
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("output %q: %v", text, err)
	}
}

func TestDescribeLastReportsTheLastEntry(t *testing.T) {
	empty := runHelper(t, "describe-last", "--dir", dataDir(t))
	if empty.code != 0 || empty.stdout != "{\"index\":0,\"term\":0}\n" {
		t.Fatalf("describe-last on a directory without entries: %+v", empty)
	}
	dir, _, _, _ := stoppedSource(t)
	got := runHelper(t, "describe-last", "--dir", dir)
	if got.code != 0 || got.stderr != "" {
		t.Fatalf("describe-last: %+v", got)
	}
	var last struct {
		Index uint64 `json:"index"`
		Term  uint64 `json:"term"`
	}
	decodeExactly(t, got.stdout, &last)
	// Three statements were committed after the node's own first entries.
	if last.Term == 0 || last.Index < 4 {
		t.Fatalf("last entry = %+v after three committed writes", last)
	}
	again := runHelper(t, "describe-last", "--dir", dir)
	if again.stdout != got.stdout {
		t.Fatalf("describe-last changed the directory: %q then %q", got.stdout, again.stdout)
	}
}

func TestInspectReportsTheSchemaDigestOfTheOriginalVoter(t *testing.T) {
	dir, address, id, schema := stoppedSource(t)
	args := func(id uint64, address, name string) []string {
		return []string{"inspect", "--dir", dir, "--id", strconv.FormatUint(id, 10), "--address", address, "--name", name}
	}
	got := runHelper(t, args(id, address, offlineDatabase)...)
	if got.code != 0 || got.stderr != "" {
		t.Fatalf("inspect: %+v", got)
	}
	var result struct {
		Schema string `json:"schema"`
	}
	decodeExactly(t, got.stdout, &result)
	if result.Schema != schema {
		t.Fatalf("schema digest %q, the server computes %q", result.Schema, schema)
	}
	for name, bad := range map[string][]string{
		"another address":   args(id, freeAddress(t), offlineDatabase),
		"another id":        args(id+1, address, offlineDatabase),
		"an empty database": args(id, address, "absent.db"),
	} {
		refused := runHelper(t, bad...)
		if refused.code != 1 || refused.stdout != "" || refused.stderr == "" {
			t.Fatalf("inspect with %s: %+v", name, refused)
		}
	}
}

func TestIsolatedServesTheNodeThroughASocketTheServerCanDial(t *testing.T) {
	dir, address, id, _ := stoppedSource(t)
	cmd := helperCommand(context.Background(), "isolated", "--dir", dir, "--id", strconv.FormatUint(id, 10),
		"--address", address, "--bind", "@", "--reject", "offline export rejects outbound raft")
	node := startAnnounced(t, cmd)
	var announced struct {
		Bind string `json:"bind"`
	}
	decodeExactly(t, node.line, &announced)
	if len(announced.Bind) < 2 || announced.Bind[0] != '@' {
		t.Fatalf("announced socket %q is not an abstract Unix socket", announced.Bind)
	}
	dial := func(ctx context.Context, requested string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", announced.Bind)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := client.New(ctx, address, client.WithDialFunc(dial))
	if err != nil {
		t.Fatal(err)
	}
	members, err := conn.Cluster(ctx)
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].ID != id || members[0].Address != address {
		t.Fatalf("members through the front socket = %+v", members)
	}
	db := openSQL(t, address, dial)
	var value string
	var count int
	if err := db.QueryRowContext(ctx, "SELECT value, (SELECT count(*) FROM settings) FROM settings WHERE key = 'b'").Scan(&value, &count); err != nil {
		t.Fatal(err)
	}
	if value != "2" || count != 2 {
		t.Fatalf("read %q of %d rows through the front socket", value, count)
	}
	// Several connections at once, as an export's pool opens them.
	for i := 0; i < 4; i++ {
		extra, err := client.New(ctx, address, client.WithDialFunc(dial))
		if err != nil {
			t.Fatal(err)
		}
		defer extra.Close()
		if _, err := extra.Leader(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Stopping must not wait for the server to hang up first.
	node.stop(t, syscall.SIGTERM)
	if _, err := (&net.Dialer{Timeout: time.Second}).Dial("unix", announced.Bind); err == nil {
		t.Fatal("the front socket outlived the helper")
	}
}

func TestReconfigureWritesTheGivenMembership(t *testing.T) {
	dir, address, id, schema := stoppedSource(t)
	moved := freeAddress(t)
	single := runHelper(t, "reconfigure", "--dir", dir, "--id", strconv.FormatUint(id, 10), "--address", moved)
	if single.code != 0 || single.stdout != "" {
		t.Fatalf("reconfigure: %+v", single)
	}
	inspect := func(address string) result {
		return runHelper(t, "inspect", "--dir", dir, "--id", strconv.FormatUint(id, 10), "--address", address, "--name", offlineDatabase)
	}
	if got := inspect(moved); got.code != 0 || !strings.Contains(got.stdout, schema) {
		t.Fatalf("inspect at the rewritten address: %+v", got)
	}
	if got := inspect(address); got.code != 1 {
		t.Fatalf("the replaced address is still the member: %+v", got)
	}
	// Two voters: one node alone is no longer the whole cluster, so the
	// single-member inspection must refuse it.
	multi := runHelper(t, "reconfigure", "--dir", dir,
		"--member", strconv.FormatUint(id, 10)+","+moved+",voter",
		"--member", strconv.FormatUint(id+1, 10)+","+freeAddress(t)+",voter")
	if multi.code != 0 || multi.stdout != "" {
		t.Fatalf("multi-member reconfigure: %+v", multi)
	}
	if got := inspect(moved); got.code != 1 || got.stdout != "" {
		t.Fatalf("a two-voter configuration passed the single-member inspection: %+v", got)
	}
}
