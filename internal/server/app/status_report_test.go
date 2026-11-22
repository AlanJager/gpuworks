//go:build linux && dqlite

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStatusReportsIdentityOnlySpareWithoutReplicationLag(t *testing.T) {
	database := closedStatusPort(t)
	peer := closedStatusPort(t)
	enrollment := closedStatusPort(t)
	root := writeStatusRaft(t, statusIdentity(7, database, 0), statusMembers(
		statusIdentity(7, database, 2),
		statusIdentity(8, peer, 0),
	), map[string]string{
		"dqlite-lock": "",
	})
	writeStatusEnrollment(t, root, enrollment)
	before := statusTreeDigest(t, root)

	text, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if statusTreeDigest(t, root) != before {
		t.Fatal("status changed the data directory")
	}
	for _, line := range []string{
		"Member: 7 " + database,
		"Cluster: " + strings.Repeat("a", 32),
		"Storage tier: standard (leader election after 150ms to 300ms without a leader)",
		"Role: spare (cluster.yaml cache)",
		"Voters: 1",
		"Fault tolerant: false",
		"persisted raft directory has only identity files",
		"database address will not listen",
		"material route will not be mounted",
		"Sync: not replicating",
		"HTTP: not observed",
		"Enrollment " + enrollment + ": not listening",
		"Database " + database + ": not listening",
		"Business: not observed",
		"materials_ready: not observed",
		"Leader: not observed",
		"Term: not observed",
		"Commit index: not observed",
		"Configuration index: not observed",
		"  7 " + database + " spare (cluster.yaml cache)",
		"  8 " + peer + " voter (cluster.yaml cache)",
		"  8 " + peer + " database: connection refused enrollment: not observed material route: not observed",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q\n%s", line, text)
		}
	}
	if strings.Contains(text, "Commit index: 0") || strings.Contains(text, "Seconds_Behind") || strings.Contains(text, "Fault tolerant: true") {
		t.Fatalf("identity-only status invented replication or fault tolerance\n%s", text)
	}
}

func TestStatusFaultToleranceRequiresThreeVotersIncludingSelf(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members string
		self    int
		voters  int
		role    string
		ready   bool
	}{
		{"two voters", statusMembers(statusIdentity(1, "127.0.0.1:1", 0), statusIdentity(2, "127.0.0.1:2", 0)), 1, 2, "voter", false},
		{"three voters", statusMembers(statusIdentity(1, "127.0.0.1:1", 0), statusIdentity(2, "127.0.0.1:2", 0), statusIdentity(3, "127.0.0.1:3", 0)), 1, 3, "voter", true},
		{"spare beside three voters", statusMembers(statusIdentity(1, "127.0.0.1:1", 2), statusIdentity(2, "127.0.0.1:2", 0), statusIdentity(3, "127.0.0.1:3", 0), statusIdentity(4, "127.0.0.1:4", 0)), 1, 3, "spare", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeStatusRaft(t, statusIdentity(tc.self, "127.0.0.1:1", 0), tc.members, map[string]string{
				"metadata1": "m", "metadata2": "m", "0000000000000001-0000000000000001": "s",
			})
			text, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, "", 200*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			ready := "false"
			if tc.ready {
				ready = "true"
			}
			for _, line := range []string{
				"Role: " + tc.role + " (cluster.yaml cache)",
				"Voters: " + strconv.Itoa(tc.voters),
				"Fault tolerant: " + ready,
				"Sync: not observed",
				"Commit index: not observed",
				"persisted raft directory has metadata and a log or snapshot",
			} {
				if !strings.Contains(text, line) {
					t.Fatalf("missing %q\n%s", line, text)
				}
			}
			if strings.Contains(text, "only identity files") || strings.Contains(text, "Commit index: 0") {
				t.Fatalf("raft directory was reported as unreplicated or lagging by zero\n%s", text)
			}
		})
	}
}

func TestStatusClassifiesPeerDatabasePorts(t *testing.T) {
	self := closedStatusPort(t)
	open, stop := openStatusPort(t)
	defer stop()
	closed := closedStatusPort(t)
	root := writeStatusRaft(t, statusIdentity(1, self, 0), statusMembers(
		statusIdentity(1, self, 0),
		statusIdentity(2, open, 0),
		statusIdentity(3, closed, 2),
	), map[string]string{"metadata1": "m", "0000000000000001-0000000000000001": "s"})
	text, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"  2 " + open + " database: open enrollment: not observed material route: not observed",
		"  3 " + closed + " database: connection refused enrollment: not observed material route: not observed",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q\n%s", line, text)
		}
	}
	if strings.Contains(text, "unreachable") || strings.Contains(text, "HTTP 404") {
		t.Fatalf("refused database was reclassified\n%s", text)
	}
}

func TestStatusTCPResultUsesRefusedTimeoutAndUnreachable(t *testing.T) {
	if tcpResult(syscall.ECONNREFUSED) != "connection refused" || tcpResult(statusTimeout{}) != "timed out" || tcpResult(errors.New("no route")) != "unreachable" || tcpResult(nil) != "open" {
		t.Fatal(tcpResult(syscall.ECONNREFUSED), tcpResult(statusTimeout{}), tcpResult(errors.New("no route")))
	}
}

type statusTimeout struct{}

func (statusTimeout) Error() string   { return "timeout" }
func (statusTimeout) Timeout() bool   { return true }
func (statusTimeout) Temporary() bool { return true }

func TestStatusReadsLocalListenersAndClosedBusiness(t *testing.T) {
	database, stopDatabase := openStatusPort(t)
	defer stopDatabase()
	enrollment, stopEnrollment := openStatusPort(t)
	defer stopEnrollment()
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/readyz" {
			t.Errorf("business probe path %s", r.URL.Path)
		}
		http.Error(w, "member business service is not ready", http.StatusServiceUnavailable)
	}))
	defer business.Close()
	root := writeStatusRaft(t, statusIdentity(7, database, 0), statusMembers(statusIdentity(7, database, 0)), map[string]string{
		"metadata1": "m", "0000000000000001-0000000000000001": "s",
	})
	writeStatusEnrollment(t, root, enrollment)
	text, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, business.Listener.Addr().String(), time.Second)
	if !errors.Is(err, ErrBusinessStatusNotReady) {
		t.Fatalf("closed business status error: %v", err)
	}
	for _, line := range []string{
		"HTTP " + business.Listener.Addr().String() + ": listening",
		"Enrollment " + enrollment + ": listening",
		"Database " + database + ": listening",
		"Business: member business service is not ready",
		"Fault tolerant: false",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q\n%s", line, text)
		}
	}
}

func TestStatusRejectsADirectoryWithoutDatabaseIdentity(t *testing.T) {
	_, err := ObserveClusterStatus(context.Background(), Config{DataDir: t.TempDir()}, "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "database identity is not in the data directory") {
		t.Fatal(err)
	}
}

func statusIdentity(id int, address string, role int) string {
	return "ID: " + strconv.Itoa(id) + "\nAddress: " + address + "\nRole: " + strconv.Itoa(role) + "\n"
}

func statusMembers(members ...string) string {
	var b strings.Builder
	for _, member := range members {
		lines := strings.Split(strings.TrimSuffix(member, "\n"), "\n")
		for i, line := range lines {
			if i == 0 {
				b.WriteString("- " + line + "\n")
				continue
			}
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

func writeStatusRaft(t *testing.T, info, members string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "gpuworks.db.dqlite")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"info.yaml": info, "cluster.yaml": members}
	for name, data := range extra {
		files[name] = data
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func writeStatusEnrollment(t *testing.T, root, listen string) {
	t.Helper()
	dir := filepath.Join(root, "cluster-enrollment")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"cluster_id":"` + strings.Repeat("a", 32) + `","ca_fingerprint":"` + strings.Repeat("b", 64) + `","protocol":1,"listen_address":"` + listen + `","public_url":"https://` + listen + `"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// closedStatusPort is an address that refuses connections for the whole test.
// The socket is bound and never listens, so a connection to it is refused,
// and it is kept open, so the port cannot be handed to anything else. A port
// that was only listened on and released can be given to the next listener
// in this process or any other, and then answers instead of refusing.
func closedStatusPort(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	bound, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(bound.(*syscall.SockaddrInet4).Port))
}

func openStatusPort(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener.Addr().String(), func() { _ = listener.Close() }
}

func statusTreeDigest(t *testing.T, root string) string {
	t.Helper()
	sum := sha256.New()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = sum.Write([]byte(path))
		_, _ = sum.Write(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func TestStatusReportsTheRecordedStorageTier(t *testing.T) {
	database, enrollment := closedStatusPort(t), closedStatusPort(t)
	root := writeStatusRaft(t, statusIdentity(7, database, 0), statusMembers(statusIdentity(7, database, 0)), map[string]string{"dqlite-lock": ""})
	writeStatusEnrollment(t, root, enrollment)
	path := filepath.Join(root, "cluster-enrollment", "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tiered := strings.Replace(string(raw), `{"cluster_id"`, `{"storage_tier":"slow-storage","cluster_id"`, 1)
	if err := os.WriteFile(path, []byte(tiered), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, "", time.Second)
	if err != nil || !strings.Contains(text, "Storage tier: slow-storage (leader election after 750ms to 1.5s without a leader)") {
		t.Fatalf("status lacks the slow-storage tier (%v)\n%s", err, text)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(tiered, "slow-storage", "glacial", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, "", time.Second); err == nil {
		t.Fatal("status reported a cluster whose recorded tier is unknown")
	}
}
