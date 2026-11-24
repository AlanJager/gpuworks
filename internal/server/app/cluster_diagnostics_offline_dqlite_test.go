//go:build linux && dqlite

package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
)

func archiveFiles(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		_, name, _ := strings.Cut(h.Name, "/")
		out[name] = string(body)
	}
}

// A member's data directory as the cluster installation leaves it: the raft
// directory, the enrollment configuration, the log beside the data, and no
// gpuworks.db. Its two peers do not answer.
func offlineMember(t *testing.T, now time.Time) (Config, string) {
	t.Helper()
	self, peerA, peerB := closedStatusPort(t), closedStatusPort(t), closedStatusPort(t)
	root := writeStatusRaft(t, statusIdentity(1, self, 0), statusMembers(
		statusIdentity(1, self, 0), statusIdentity(2, peerA, 0), statusIdentity(3, peerB, 0),
	), map[string]string{"dqlite-lock": ""})
	writeStatusEnrollment(t, root, closedStatusPort(t))
	logFile := filepath.Join(root, "server.log")
	at := now.Add(-5 * time.Minute).Format(time.RFC3339Nano)
	lines := "time=" + at + ` level=WARN msg="cluster database role event" observed_at=` + at + " event=loop_late late_ms=1819 period_ms=75\n" +
		"time=" + at + ` level=INFO msg="cluster database leader changed" old_leader=1 new_leader=0 term=7 this_member=1 leads=false reason="this member stepped down: no_majority_contact"` + "\n"
	if err := os.WriteFile(logFile, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "gpuworks.db")); err == nil {
		t.Fatal("the fixture has a gpuworks.db")
	}
	return Config{DataDir: root, LogFile: logFile}, self
}

// With no leader and no peer answering, the member's own records are still
// collected, the missing members are named, and the records the database
// holds are marked as not collected.
func TestAMemberCollectsWithoutItsDatabaseWhileTheClusterDoesNotServe(t *testing.T) {
	now := time.Now().UTC()
	cfg, self := offlineMember(t, now)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := ClusterMemberDiagnosticsRequest{DataDir: cfg.DataDir, LogFile: cfg.LogFile, WorkDir: t.TempDir(), Request: diagnostics.BundleRequest{RequestedBy: "support-bundle", From: now.Add(-time.Hour), To: now.Add(time.Minute)}}
	began := time.Now()
	bundle, archive, err := CollectClusterMemberDiagnostics(ctx, req, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || bundle.State != "ready" {
		t.Fatalf("collection = %q, %v", bundle.State, err)
	}
	// Nothing here waits for a leader or opens the replicated store: with
	// every peer down the probes are refused and the collection is done.
	if took := time.Since(began); took > 20*time.Second {
		t.Fatalf("the collection took %v with no leader", took)
	}
	files := archiveFiles(t, archive)
	if !strings.Contains(files["server/gpuworks-server.log"], "event=loop_late") {
		t.Fatalf("own log:\n%s", files["server/gpuworks-server.log"])
	}
	var index diagnostics.MemberIndex
	if err := json.Unmarshal([]byte(files["members/index.json"]), &index); err != nil {
		t.Fatal(err)
	}
	if index.Complete || len(index.Members) != 3 {
		t.Fatalf("index = %+v, want three members and an incomplete collection", index)
	}
	missing := 0
	for _, member := range index.Members {
		switch {
		case member.Self:
			if member.Missing != "" || member.Address != self {
				t.Fatalf("own entry = %+v", member)
			}
			if got := files["members/"+member.Name+"/role-events.log"]; !strings.Contains(got, "event=loop_late late_ms=1819") {
				t.Fatalf("own role events:\n%s", got)
			}
			if got := files["members/"+member.Name+"/leader-changes.log"]; !strings.Contains(got, "old_leader=1 new_leader=0") {
				t.Fatalf("own leader changes:\n%s", got)
			}
			if got := files["members/"+member.Name+"/cluster-status.txt"]; !strings.Contains(got, "Leader: not observed") {
				t.Fatalf("own status:\n%s", got)
			}
		case member.Missing == "" || member.Evidence == "":
			t.Fatalf("a member that did not answer = %+v", member)
		default:
			missing++
		}
	}
	if missing != 2 {
		t.Fatalf("%d members missing, want 2", missing)
	}
	// What only the database holds is named as not collected, with why.
	found := false
	for _, item := range bundle.Items {
		if item.Status == proto.CheckFail && strings.Contains(item.Evidence, "cluster database") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no item says the database records are missing: %+v", bundle.Items)
	}

	req.Local, req.WorkDir = true, t.TempDir()
	bundle, archive, err = CollectClusterMemberDiagnostics(ctx, req, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || bundle.State != "ready" {
		t.Fatalf("local collection = %q, %v", bundle.State, err)
	}
	index = diagnostics.MemberIndex{}
	_ = json.Unmarshal([]byte(archiveFiles(t, archive)["members/index.json"]), &index)
	if !index.Complete || len(index.Members) != 1 || !index.Members[0].Self {
		t.Fatalf("local index = %+v, want this member alone", index)
	}
}

func TestOfflineMemberCollectionRefusesADirectoryThatIsNoMembers(t *testing.T) {
	_, _, err := CollectClusterMemberDiagnostics(context.Background(), ClusterMemberDiagnosticsRequest{DataDir: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "not a control-plane cluster member") {
		t.Fatalf("err = %v", err)
	}
}

// A member directory whose raft identity cannot be read is still a member's:
// its membership is an error naming what could not be read, never an empty
// list, which would make its collection read as a single server's.
func TestAMemberWhoseMembershipCannotBeReadSaysSo(t *testing.T) {
	cfg, _ := offlineMember(t, time.Now().UTC())
	info := filepath.Join(cfg.DataDir, "gpuworks.db.dqlite", "info.yaml")
	if err := os.Remove(info); err != nil {
		t.Fatal(err)
	}
	cluster := newClusterDiagnostics(cfg)
	if cluster == nil {
		t.Fatal("the directory is no longer taken for a member's")
	}
	members, err := cluster.Members(t.Context())
	if err == nil || len(members) != 0 || !strings.Contains(err.Error(), "this member's cluster membership could not be read: ") {
		t.Fatalf("members = %v, err = %v", members, err)
	}
	bundle, archive, err := CollectClusterMemberDiagnostics(t.Context(), ClusterMemberDiagnosticsRequest{DataDir: cfg.DataDir, LogFile: cfg.LogFile, WorkDir: t.TempDir(),
		Request: diagnostics.BundleRequest{RequestedBy: "support-bundle"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || bundle.State != "ready" {
		t.Fatalf("collection = %q, %v", bundle.State, err)
	}
	var item diagnostics.BundleItem
	for _, it := range bundle.Items {
		if it.Path == "members" {
			item = it
		}
	}
	if item.Status != proto.CheckFail || item.ID() != diagnostics.BundleItemMembers || !strings.Contains(item.Evidence, "could not be read") {
		t.Fatalf("members item = %+v", item)
	}
	if _, ok := archiveFiles(t, archive)["members/index.json"]; ok {
		t.Fatal("an index was written without a membership")
	}
}
