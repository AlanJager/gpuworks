//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func TestNativeQuorumProofRequiresFreshThreeVoterBarrier(t *testing.T) {
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
	conversionWait(t, ctx, "actual three-voter proof configuration", func() bool {
		members, err := dbs[0].Members(ctx)
		return err == nil && len(members) == 3 && members[0].Role == "voter" && members[1].Role == "voter" && members[2].Role == "voter"
	})
	baseline := settledQuorumProof(t, ctx, "actual Database quorum-proof capability", dbs[0])
	leaderIndex := -1
	for i, db := range dbs {
		if db.ID == baseline.LeaderID {
			leaderIndex = i
		}
	}
	if leaderIndex < 0 {
		t.Fatalf("actual proof leader is not a fixture member: %+v", baseline)
	}
	leader := dbs[leaderIndex]
	materialQuorumSeedSQL(t, ctx, leader.SQL, fixtures)
	originalSQL := materialQuorumSQLSnapshot(t, ctx, leader.SQL)
	beforeMembers, err := leader.Members(ctx)
	if err != nil {
		t.Fatal(err)
	}
	materialQuorumAssertProof(t, baseline, beforeMembers)
	stopPeers := func(t *testing.T) {
		t.Helper()
		for i, db := range dbs {
			if i != leaderIndex && !db.closed.Load() {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	t.Run("fresh-index-and-unchanged-sql", func(t *testing.T) {
		first, err := leader.QuorumProof(ctx)
		if err != nil {
			t.Fatal(err)
		}
		second, err := leader.QuorumProof(ctx)
		if err != nil {
			t.Fatal(err)
		}
		materialQuorumAssertProof(t, first, beforeMembers)
		materialQuorumAssertProof(t, second, beforeMembers)
		if first.Index <= baseline.Index || second.Index <= first.Index || second.Term < first.Term {
			t.Fatalf("native proof cached/merged or regressed: baseline=%+v first=%+v second=%+v", baseline, first, second)
		}
		members, err := leader.Members(ctx)
		if err != nil || !reflect.DeepEqual(materialQuorumSorted(members), materialQuorumSorted(beforeMembers)) {
			t.Fatalf("ordinary V1 Cluster changed after proof: members=%+v err=%v", members, err)
		}
		if got := materialQuorumSQLSnapshot(t, ctx, leader.SQL); !reflect.DeepEqual(got, originalSQL) {
			t.Fatal("fresh native barriers wrote representative business/admission/schema/sequence/audit SQL")
		}
		t.Logf("REAL_NATIVE_QUORUM baseline=%+v first=%+v second=%+v V1=%+v", baseline, first, second, members)
	})
	t.Run("cached-membership-without-quorum-is-not-proof", func(t *testing.T) {
		stopPeers(t)
		short, done := context.WithTimeout(ctx, time.Second)
		cachedLeader, leaderErr := leader.Leader(short)
		done()
		short, done = context.WithTimeout(ctx, time.Second)
		cachedClient, err := client.New(short, leader.Address, client.WithDialFunc(materialQuorumTLS(t, trust, policy, fixtures[leaderIndex])))
		if err != nil {
			done()
			t.Fatal("actual V1 Cluster direct TLS connection", err)
		}
		nativeMembers, membersErr := cachedClient.Cluster(short)
		closeErr := cachedClient.Close()
		done()
		if closeErr != nil {
			t.Fatal("actual V1 Cluster connection Close", closeErr)
		}
		cachedMembers := make([]Member, 0, len(nativeMembers))
		for _, member := range nativeMembers {
			cachedMembers = append(cachedMembers, Member{ID: member.ID, Address: member.Address, Role: member.Role.String()})
		}
		t.Logf("NO_QUORUM_V1 locator=%+v err=%v members=%+v err=%v", cachedLeader, leaderErr, cachedMembers, membersErr)
		if membersErr != nil || !reflect.DeepEqual(materialQuorumSorted(cachedMembers), materialQuorumSorted(beforeMembers)) {
			t.Fatalf("actual V1 cached configuration was not observable without quorum: %+v %v", cachedMembers, membersErr)
		}
		short, done = context.WithTimeout(ctx, time.Second)
		started := time.Now()
		proof, err := leader.QuorumProof(short)
		done()
		if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) || time.Since(started) > 3*time.Second {
			t.Fatalf("actual fresh proof accepted the offline majority or exceeded its bound: proof=%+v err=%v elapsed=%s", proof, err, time.Since(started))
		}
		t.Logf("NO_QUORUM_FRESH_REJECTION=%v", err)
	})
	t.Run("actual-client-cancel-closes-connection-and-native-recovers", func(t *testing.T) {
		stopPeers(t)
		requestCtx, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		var observed *materialQuorumObservedConnection
		dial := materialQuorumTLS(t, trust, policy, fixtures[leaderIndex])
		proof, err := ReadQuorumProof(requestCtx, leader.Address, func(callCtx context.Context, address string) (net.Conn, error) {
			conn, err := dial(callCtx, address)
			if err != nil {
				return nil, err
			}
			observed = &materialQuorumObservedConnection{Conn: conn, cancel: cancelRequest, closed: make(chan struct{})}
			return observed, nil
		})
		if observed == nil || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("actual post-frame cancellation did not fail closed: conn=%v proof=%+v err=%v", observed != nil, proof, err)
		}
		select {
		case <-observed.closed:
		default:
			t.Fatal("protocol caller returned before actual connection Close finished")
		}
		observed.mu.Lock()
		written := append([]byte(nil), observed.written...)
		closes, closeErr := observed.closes, observed.closeErr
		observed.mu.Unlock()
		if !bytes.Equal(written, materialQuorumRequest) || closes != 1 || closeErr != nil {
			t.Fatalf("actual authenticated frame/Close facts: frame=%x closes=%d closeErr=%v", written, closes, closeErr)
		}
		actualTLS, ok := observed.Conn.(*tls.Conn)
		if !ok {
			t.Fatal("actual TLS connection was not retained by observer")
		}
		if _, err := actualTLS.NetConn().Write([]byte{0}); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("actual underlying native TCP connection remains writable: %v", err)
		}
		// Restart both original peers before waiting for SQL: one alone cannot
		// establish a majority. Existing IDs/policy/native files are preserved.
		for i := range dbs {
			if i == leaderIndex {
				continue
			}
			db, err := StartAuthorizedDatabase(ctx, fixtures[i].config, fixtures[i].policyPath, trust, fixtures[i].certificate, fixtures[i].key)
			if err != nil {
				t.Fatal("original voter restart", err)
			}
			if db.ID != dbs[i].ID {
				t.Fatalf("native recovery replaced voter identity %d -> %d", dbs[i].ID, db.ID)
			}
			dbs[i] = db
			t.Cleanup(func() { _ = db.Close() })
		}
		for _, db := range dbs {
			if err := db.WaitReady(ctx); err != nil {
				t.Fatal("restored actual native quorum", err)
			}
		}
		// While its peers were away the leader's role keeper may have started
		// a membership change that only commits once they are back. A proof is
		// refused, by design, until that change has settled; any other refusal
		// is a failure.
		recovered := settledQuorumProof(t, ctx, "native process/late callbacks did not permit recovered proof", leader)
		materialQuorumAssertProof(t, recovered, beforeMembers)
		if recovered.Index <= baseline.Index || recovered.Term < baseline.Term {
			t.Fatalf("recovered actual proof regressed: baseline=%+v recovered=%+v", baseline, recovered)
		}
		if got := materialQuorumSQLSnapshot(t, ctx, leader.SQL); !reflect.DeepEqual(got, originalSQL) {
			t.Fatal("native cancellation/recovery changed representative SQL state")
		}
		t.Logf("ACTUAL_CANCELED_REQUEST=%x CLOSES=%d RECOVERED_PROOF=%+v", written, closes, recovered)
	})
}

func materialQuorumAssertProof(t *testing.T, proof QuorumProof, expected []Member) {
	t.Helper()
	if proof.Term == 0 || proof.Index == 0 || proof.ConfigurationIndex == 0 || proof.ConfigurationIndex > proof.Index || proof.LeaderID == 0 || !reflect.DeepEqual(materialQuorumSorted(proof.Members), materialQuorumSorted(expected)) {
		t.Fatalf("actual committed native quorum proof fields/configuration invalid: %+v expected=%+v", proof, expected)
	}
	leader := false
	for _, member := range proof.Members {
		if member.ID == 0 || member.Address == "" || member.Role != "voter" {
			t.Fatalf("native proof did not contain actual voter: %+v", member)
		}
		leader = leader || member.ID == proof.LeaderID
	}
	if !leader || len(proof.Members) != 3 {
		t.Fatalf("actual native proof omitted its three-voter leader: %+v", proof)
	}
}

func materialQuorumSorted(members []Member) []Member {
	result := append([]Member(nil), members...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func materialQuorumTLS(t *testing.T, trust *identity.Trust, policy identity.TransportPolicy, member authorizedMemberFixture) func(context.Context, string) (net.Conn, error) {
	t.Helper()
	authorize, err := trust.PolicyAuthorizer(policy)
	if err != nil {
		t.Fatal(err)
	}
	_, config, err := trust.TLSConfigs(member.certificate, member.key, time.Now().UTC(), authorize)
	if err != nil {
		t.Fatal(err)
	}
	verify := config.VerifyConnection
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if err := verify(state); err != nil {
			return err
		}
		key, err := trust.VerifyMember(state.PeerCertificates[0], time.Now().UTC())
		if err != nil || key != member.fingerprint {
			return errors.New("actual proof TLS endpoint member key differs")
		}
		return nil
	}
	return func(ctx context.Context, address string) (net.Conn, error) {
		if address != member.config.Address {
			return nil, errors.New("actual proof refuses endpoint redirection")
		}
		return (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", address)
	}
}

var materialQuorumRequest = []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 16, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0}

// This observer retains the actual TLS connection and request bytes/Close
// completion, which cannot be inferred from an error or a canceled context.
type materialQuorumObservedConnection struct {
	net.Conn
	cancel   context.CancelFunc
	mu       sync.Mutex
	written  []byte
	closes   int
	closeErr error
	closed   chan struct{}
}

func (c *materialQuorumObservedConnection) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	c.written = append(c.written, p[:n]...)
	complete := bytes.Equal(c.written, materialQuorumRequest)
	c.mu.Unlock()
	if complete && err == nil {
		c.cancel()
	}
	return n, err
}

func (c *materialQuorumObservedConnection) Close() error {
	err := c.Conn.Close()
	c.mu.Lock()
	c.closes++
	c.closeErr = err
	c.mu.Unlock()
	close(c.closed)
	return err
}

func materialQuorumSeedSQL(t *testing.T, ctx context.Context, db *sql.DB, members []authorizedMemberFixture) {
	t.Helper()
	for _, query := range []string{
		`CREATE TABLE quorum_resources(id INTEGER PRIMARY KEY AUTOINCREMENT, uuid TEXT, token_hash TEXT, data BLOB)`,
		`INSERT INTO quorum_resources VALUES(1,'original-resource','fixture-token-hash',X'006162FF')`,
		`INSERT INTO quorum_resources VALUES(1000,'removed-resource','fixture-token-hash',X'00')`,
		`DELETE FROM quorum_resources WHERE id=1000`,
		`CREATE TABLE quorum_admissions(key_fingerprint TEXT PRIMARY KEY, state TEXT, address TEXT)`,
		`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_migrations VALUES(17)`,
		`CREATE TABLE quorum_sql_writes(id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT)`,
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal("native SQL state fixture", err)
		}
	}
	for _, member := range members {
		if _, err := db.ExecContext(ctx, `INSERT INTO quorum_admissions VALUES(?,?,?)`, member.fingerprint, "admitted", member.config.Address); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"quorum_resources", "quorum_admissions", "schema_migrations"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			query := fmt.Sprintf(`CREATE TRIGGER audit_%s_%s AFTER %s ON %s BEGIN INSERT INTO quorum_sql_writes(kind) VALUES('%s:%s'); END`, table, event, event, table, table, event)
			if _, err := db.ExecContext(ctx, query); err != nil {
				t.Fatal("actual SQL mutation witness", err)
			}
		}
	}
}

func materialQuorumSQLSnapshot(t *testing.T, ctx context.Context, db *sql.DB) map[string][][]string {
	t.Helper()
	queries := map[string]string{
		"schema":      `SELECT type,name,tbl_name,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`,
		"business":    `SELECT id,uuid,token_hash,hex(data) FROM quorum_resources ORDER BY id`,
		"admission":   `SELECT key_fingerprint,state,address FROM quorum_admissions ORDER BY key_fingerprint`,
		"markers":     `SELECT version FROM schema_migrations ORDER BY version`,
		"sequences":   `SELECT name,seq FROM sqlite_sequence ORDER BY name`,
		"write-audit": `SELECT id,kind FROM quorum_sql_writes ORDER BY id`,
	}
	result := make(map[string][][]string, len(queries))
	for label, query := range queries {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal("actual native SQL snapshot", label, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		result[label] = nil
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			line := make([]string, len(values))
			for i, value := range values {
				if body, ok := value.([]byte); ok {
					line[i] = fmt.Sprintf("bytes:%x", body)
				} else {
					line[i] = fmt.Sprintf("%T:%v", value, value)
				}
			}
			result[label] = append(result[label], line)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
	}
	if len(result["write-audit"]) != 0 {
		t.Fatalf("actual representative SQL mutation witness is not empty: %v", result["write-audit"])
	}
	return result
}

// settledQuorumProof reads a proof once the membership has settled. The
// members already list as voters while the configuration entry that made the
// last one a voter is still uncommitted, or its promotion still in progress;
// the database process refuses a proof until then, by design, with "quorum
// configuration is changing". That refusal is waited out within ctx; any other
// error fails the test.
func settledQuorumProof(t *testing.T, ctx context.Context, description string, db *Database) QuorumProof {
	t.Helper()
	var proof QuorumProof
	conversionWait(t, ctx, description+": membership change still pending", func() bool {
		var err error
		proof, err = db.QuorumProof(ctx)
		if err != nil && !strings.Contains(err.Error(), "quorum configuration is changing") {
			t.Fatal(description, err)
		}
		return err == nil
	})
	return proof
}
