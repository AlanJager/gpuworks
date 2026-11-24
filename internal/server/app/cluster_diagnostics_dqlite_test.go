//go:build linux && dqlite

package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// memberPEM is a fixture pair as a member keeps it on disk.
func memberPEM(t *testing.T, pair tls.Certificate) ([]byte, []byte) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}

// writeMemberIdentity lays out a member's cluster-enrollment directory.
func writeMemberIdentity(t *testing.T, f *closedFactsTLSFixture, root, listen string, pair tls.Certificate) {
	t.Helper()
	dir := filepath.Join(root, "cluster-enrollment")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cert, key := memberPEM(t, pair)
	config := `{"cluster_id":"` + f.bundle.info.ClusterID + `","ca_fingerprint":"` + f.bundle.info.CAFingerprint + `","protocol":1,"listen_address":"` + listen + `","public_url":"https://` + listen + `"}`
	for name, data := range map[string][]byte{"config.json": []byte(config), "ca.pem": f.bundle.ca, "server.pem": cert, "server.key": key} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func fixtureMembers() string {
	return statusMembers(statusIdentity(7, "127.0.0.1:9910", 0), statusIdentity(9, "127.0.0.1:9911", 0), statusIdentity(11, "127.0.0.1:9912", 0))
}

// diagnosticsMember is the answering member (member 7 of the fixture): a
// data directory with its raft identity, a log, a secret beside it and a
// transport policy naming the keys it admits.
func diagnosticsMember(t *testing.T, f *closedFactsTLSFixture, now time.Time, revoked ...string) (*enrollmentServer, string) {
	t.Helper()
	return diagnosticsMemberAt(t, f, now, "gpuworks.db", revoked...)
}

// diagnosticsMemberAt is diagnosticsMember with its database under another
// name, as a member that formed the cluster may have it.
func diagnosticsMemberAt(t *testing.T, f *closedFactsTLSFixture, now time.Time, database string, revoked ...string) (*enrollmentServer, string) {
	t.Helper()
	data := writeStatusRaft(t, statusIdentity(7, "127.0.0.1:9910", 0), fixtureMembers(), map[string]string{"dqlite-lock": ""})
	local := Config{DataDir: data, DBPath: filepath.Join(data, database)}
	if database != "gpuworks.db" {
		if err := os.Rename(filepath.Join(data, "gpuworks.db.dqlite"), local.DBPath+".dqlite"); err != nil {
			t.Fatal(err)
		}
	}
	writeMemberIdentity(t, f, data, closedStatusPort(t), f.serverPair)
	f.bundle.dataDir = data
	logFile := filepath.Join(data, "logs", "server.log")
	_ = os.MkdirAll(filepath.Dir(logFile), 0o700)
	at := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
	lines := "time=" + at + ` level=INFO msg="cluster database role event" observed_at=` + at + " event=step_down reason=no_majority_contact contacts=1 voters=3 term=7\n" +
		"time=" + at + ` level=INFO msg="cluster database leader changed" old_leader=7 new_leader=9 term=8 this_member=7 leads=false reason="this member stepped down: no_majority_contact" without_leader_ms=1493` + "\n" +
		"time=" + now.Add(-30*time.Hour).Format(time.RFC3339Nano) + ` level=INFO msg="outside the window"` + "\n"
	if err := os.WriteFile(logFile, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "master.key"), []byte("MASTERKEY-5f2b7c"), 0o600); err != nil {
		t.Fatal(err)
	}
	allowed := []string{f.serverKey, f.requesterKey, f.otherKey}
	sort.Strings(allowed)
	policy := identity.TransportPolicy{ClusterID: f.bundle.info.ClusterID, CAFingerprint: f.bundle.info.CAFingerprint, Revision: 1, RevokedKeys: revoked}
	for _, key := range allowed {
		if len(revoked) == 0 || key != revoked[0] {
			policy.AllowedKeys = append(policy.AllowedKeys, key)
		}
	}
	path, _, err := ClusterTransportPolicyPath(local)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := identity.PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	local.LogFile = logFile
	server, err := newClosedFactsEnrollmentServer(f.bundle, f.session, local, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	return server, data
}

func diagnosticsClient(t *testing.T, f *closedFactsTLSFixture, server *enrollmentServer) *enrollment.Client {
	t.Helper()
	client, err := enrollment.NewClient("https://"+server.Addr()+enrollmentCertificatePath, f.bundle.info.ClusterID, f.bundle.info.CAFingerprint, store.ClusterProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// partArchive reads a part's files.
func partArchive(t *testing.T, body io.Reader) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(body)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(tr)
		files[h.Name] = string(raw)
	}
}

// A member gives its part to another member over the enrollment listener's
// mutual TLS, with no database, no leader and its business closed.
func TestAMemberGivesItsDiagnosticsPartToAnAdmittedMember(t *testing.T) {
	now := time.Now().UTC()
	f := newClosedFactsTLSFixture(t)
	server, _ := diagnosticsMember(t, f, now)
	cert, key := memberPEM(t, f.requesterPair)
	answer, err := diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	if d := answer.Clock.Sub(answer.Sent); answer.Clock.IsZero() || d < 0 || d > answer.RoundTrip {
		t.Fatalf("member clock = %v, sent %v, round trip %v: on one host the clock lies inside the round trip", answer.Clock, answer.Sent, answer.RoundTrip)
	}
	if answer.MemberKey != f.serverKey {
		t.Fatalf("answered by key %s, want the member's %s", answer.MemberKey, f.serverKey)
	}
	files := partArchive(t, answer.Body)
	if log := files[diagnostics.MemberLogFile]; strings.Count(log, "\n") != 2 || strings.Contains(log, "outside the window") {
		t.Fatalf("log in the part:\n%s", log)
	}
	if got := files[diagnostics.MemberRoleEventsFile]; strings.Count(got, "\n") != 1 || !strings.Contains(got, "event=step_down reason=no_majority_contact") {
		t.Fatalf("role events:\n%s", got)
	}
	if got := files[diagnostics.MemberLeaderChangesFile]; strings.Count(got, "\n") != 1 || !strings.Contains(got, "old_leader=7 new_leader=9") {
		t.Fatalf("leader changes:\n%s", got)
	}
	var facts diagnostics.MemberFacts
	if err := json.Unmarshal([]byte(files[diagnostics.MemberFactsFile]), &facts); err != nil || facts.Version == "" || facts.ServerSHA256 == "" || !strings.HasSuffix(facts.LogFile, "logs/server.log") || facts.ID != 7 || facts.Address != "127.0.0.1:9910" {
		t.Fatalf("member.json = %+v, %v", facts, err)
	}
	for name, body := range files {
		if strings.Contains(body, "MASTERKEY-5f2b7c") || strings.Contains(body, "PRIVATE KEY") {
			t.Fatalf("%s carries a secret", name)
		}
	}
}

func TestAMemberRefusesItsDiagnosticsPart(t *testing.T) {
	now := time.Now().UTC()
	from, to := now.Add(-time.Hour), now
	status := func(t *testing.T, err error) int {
		t.Helper()
		var code *enrollment.StatusError
		if !errors.As(err, &code) {
			t.Fatalf("err = %v, want a status", err)
		}
		return code.Code
	}
	t.Run("a key the policy does not name", func(t *testing.T) {
		f := newClosedFactsTLSFixture(t)
		server, _ := diagnosticsMember(t, f, now)
		cert, key := memberPEM(t, f.unknownPair)
		if _, err := diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, from, to); status(t, err) != http.StatusForbidden {
			t.Fatalf("err = %v, want 403", err)
		}
	})
	t.Run("a revoked key", func(t *testing.T) {
		f := newClosedFactsTLSFixture(t)
		server, _ := diagnosticsMember(t, f, now, f.requesterKey)
		cert, key := memberPEM(t, f.requesterPair)
		if _, err := diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, from, to); status(t, err) != http.StatusForbidden {
			t.Fatalf("err = %v, want 403", err)
		}
	})
	t.Run("a policy that cannot be read", func(t *testing.T) {
		f := newClosedFactsTLSFixture(t)
		server, data := diagnosticsMember(t, f, now)
		path, _, _ := ClusterTransportPolicyPath(Config{DataDir: data})
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		cert, key := memberPEM(t, f.requesterPair)
		if _, err := diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, from, to); status(t, err) != http.StatusForbidden {
			t.Fatalf("err = %v, want 403", err)
		}
	})
	t.Run("a window it cannot serve", func(t *testing.T) {
		f := newClosedFactsTLSFixture(t)
		server, _ := diagnosticsMember(t, f, now)
		cert, key := memberPEM(t, f.requesterPair)
		for _, w := range [][2]time.Time{{to, from}, {now.Add(-8 * 24 * time.Hour), now}} {
			if _, err := diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, w[0], w[1]); status(t, err) != http.StatusBadRequest {
				t.Fatalf("window %v: err = %v, want 400", w, err)
			}
		}
	})
	t.Run("no member certificate", func(t *testing.T) {
		f := newClosedFactsTLSFixture(t)
		server, _ := diagnosticsMember(t, f, now)
		// The listener requires a member certificate: the connection is
		// refused in the handshake, before any route.
		response, _, err := closedFactsRouteRequest(t, f.client(t, nil), server, http.MethodPost, enrollment.ClusterDiagnosticsPath, `{}`)
		if err == nil || response != nil || !strings.Contains(err.Error(), "certificate required") {
			t.Fatalf("without a member certificate: %v, %v; want the handshake refused", response, err)
		}
	})
	t.Run("another method", func(t *testing.T) {
		f := newClosedFactsTLSFixture(t)
		server, _ := diagnosticsMember(t, f, now)
		response, _, err := closedFactsRouteRequest(t, f.client(t, &f.requesterPair), server, http.MethodGet, enrollment.ClusterDiagnosticsPath, "")
		if err != nil || response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET = %v, %v; want 405", response, err)
		}
	})
}

// While the member's business serves, the prepared routes are installed on
// the listener; the diagnostics route must stay the listener's own.
func TestTheDiagnosticsRouteStaysWithTheListenerWhileBusinessServes(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	gate := newEnrollmentHandlerGate(base)
	gate.install(http.NotFoundHandler())
	response := httptest.NewRecorder()
	gate.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/_cluster/diagnostics", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("the diagnostics route answered %d behind the prepared routes", response.Code)
	}
}

// A member whose database is not at the default path still knows who it is:
// its part carries its identity, and its own policy file admits the asker.
func TestAMemberWithItsDatabaseElsewhereGivesItsPartAsItself(t *testing.T) {
	now := time.Now().UTC()
	f := newClosedFactsTLSFixture(t)
	server, data := diagnosticsMemberAt(t, f, now, "cluster.db")
	if _, err := os.Stat(filepath.Join(data, "gpuworks.db.dqlite")); err == nil {
		t.Fatal("the fixture left a database at the default path")
	}
	cert, key := memberPEM(t, f.requesterPair)
	answer, err := diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	var facts diagnostics.MemberFacts
	if err := json.Unmarshal([]byte(partArchive(t, answer.Body)[diagnostics.MemberFactsFile]), &facts); err != nil || facts.ID != 7 || facts.Address != "127.0.0.1:9910" {
		t.Fatalf("member.json = %+v, %v; want member 7", facts, err)
	}
}
