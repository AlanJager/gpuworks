//go:build linux && dqlite

package cli

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
)

// clusterMemberLayout is memberLayout with what a member of a three-member
// cluster keeps: its raft identity and membership, its enrollment
// configuration, and its log where the cluster installation writes it. It
// has no gpuworks.db and no member certificate, and nothing listens at the
// members' addresses.
func clusterMemberLayout(t *testing.T) (data string) {
	t.Helper()
	install, data := memberLayout(t)
	raft := filepath.Join(data, "gpuworks.db.dqlite")
	if err := os.Mkdir(raft, 0o700); err != nil {
		t.Fatal(err)
	}
	member := func(id string) string { return "ID: " + id + "\nAddress: 127.0.0.1:" + id + "\nRole: 0\n" }
	listed := func(id string) string {
		return "- " + strings.ReplaceAll(strings.TrimSuffix(member(id), "\n"), "\n", "\n  ") + "\n"
	}
	enrollment := `{"cluster_id":"` + strings.Repeat("a", 32) + `","ca_fingerprint":"` + strings.Repeat("b", 64) +
		`","protocol":1,"listen_address":"127.0.0.1:4","public_url":"https://127.0.0.1:4"}`
	at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	log := "time=" + at + ` level=INFO msg="cluster database role event" observed_at=` + at + " event=step_down reason=no_majority_contact\n"
	if err := os.Mkdir(filepath.Join(install, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(raft, "info.yaml"):                         member("1"),
		filepath.Join(raft, "cluster.yaml"):                      listed("1") + listed("2") + listed("3"),
		filepath.Join(raft, "dqlite-lock"):                       "",
		filepath.Join(data, "cluster-enrollment", "config.json"): enrollment,
		filepath.Join(install, "logs", "server.log"):             log,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "gpuworks.db")); err == nil {
		t.Fatal("the fixture has a gpuworks.db")
	}
	return data
}

func bundleFiles(t *testing.T, path string) map[string]string {
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
	files := map[string]string{}
	for tr := tar.NewReader(gz); ; {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		_, name, _ := strings.Cut(h.Name, "/")
		files[name] = string(body)
	}
}

// A member keeps no gpuworks.db; the command must not go looking for one. A
// build with the cluster runtime collects from the member's files: its own
// records, and for each member that does not answer a line saying so.
func TestSupportBundleOnAClusterMemberDoesNotLookForASQLiteDatabase(t *testing.T) {
	data := clusterMemberLayout(t)
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	code, logged, printed := operatorOutput(t, "--data", data, "--out", out)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, logged)
	}
	if strings.Count(printed, "the collection does not show what that member saw") != 2 || !strings.Contains(printed, "the collection is incomplete") ||
		!strings.Contains(printed, "members/127.0.0.1_2 is missing") || !strings.Contains(printed, "members/127.0.0.1_3 is missing") {
		t.Fatalf("the two members that do not answer are not named to the operator:\n%s\nlogged:\n%s", printed, logged)
	}
	files := bundleFiles(t, out)
	var index diagnostics.MemberIndex
	if err := json.Unmarshal([]byte(files["members/index.json"]), &index); err != nil {
		t.Fatalf("members/index.json: %v\n%s", err, files["members/index.json"])
	}
	if index.Complete || len(index.Members) != 3 {
		t.Fatalf("index = %+v, want three members and an incomplete collection", index)
	}
	for _, member := range index.Members {
		if member.Self != (member.Address == "127.0.0.1:1") || (member.Missing == "") != member.Self {
			t.Fatalf("member = %+v", member)
		}
	}
	// The log is where the cluster installation keeps it, beside the data.
	if got := files["members/127.0.0.1_1/role-events.log"]; !strings.Contains(got, "event=step_down reason=no_majority_contact") {
		t.Fatalf("this member's role records:\n%s", got)
	}
	if !strings.Contains(files["server/gpuworks-server.log"], "event=step_down") {
		t.Fatal("this member's log is not in the collection")
	}
	// What only the database holds is marked, not silently absent.
	if !strings.Contains(files["manifest.json"], "cluster database") {
		t.Fatalf("the manifest does not say the database's records are missing:\n%s", files["manifest.json"])
	}
}

func TestSupportBundleLocalCollectsThisMemberAlone(t *testing.T) {
	data := clusterMemberLayout(t)
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	code, logged, printed := operatorOutput(t, "--data", data, "--out", out, "--local")
	if code != 0 || printed != "" {
		t.Fatalf("exit %d, printed %q:\n%s", code, printed, logged)
	}
	var index diagnostics.MemberIndex
	if err := json.Unmarshal([]byte(bundleFiles(t, out)["members/index.json"]), &index); err != nil {
		t.Fatal(err)
	}
	if !index.Complete || len(index.Members) != 1 || !index.Members[0].Self {
		t.Fatalf("index = %+v, want this member alone", index)
	}
}

// A member whose cluster files cannot be read is still a member: the
// command collects its own log, tells the operator that the other members
// were not asked and why, and the manifest carries a failed item. Without
// them the archive would read as a single server's.
func TestSupportBundleSaysWhenAMembersClusterIdentityCannotBeRead(t *testing.T) {
	data := clusterMemberLayout(t)
	if err := os.Remove(filepath.Join(data, "gpuworks.db.dqlite", "info.yaml")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	code, logged, printed := operatorOutput(t, "--data", data, "--out", out)
	if code != 0 {
		t.Fatalf("exit %d: an unreadable membership must not fail the collection\n%s", code, logged)
	}
	identity := filepath.Join(data, "gpuworks.db.dqlite", "info.yaml")
	want := "support-bundle: this member's cluster membership could not be read: database identity is not in the data directory (" + identity + "); " +
		"the collection holds this member's own log, role records and stall reports, and no other member's: collect on another member\n"
	if printed != want {
		t.Fatalf("the operator is told:\n%s\nwant:\n%s\nlogged:\n%s", printed, want, logged)
	}
	files := bundleFiles(t, out)
	for path := range files {
		if strings.HasPrefix(path, "members/") {
			t.Fatalf("the collection holds %s without a membership", path)
		}
	}
	var manifest diagnostics.Bundle
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range manifest.Items {
		if item.Path == "members" {
			found = item.Status == "fail" && item.Code == "DIAGNOSTICS_ITEM_READ_FAILED" && strings.Contains(item.Evidence, identity)
		}
	}
	if !found || !strings.Contains(files["server/gpuworks-server.log"], "event=step_down") || !strings.Contains(files["server/role-events.log"], "event=step_down") {
		t.Fatalf("the manifest has no failed members item naming the file, or this member's log or role records are missing:\n%s", files["manifest.json"])
	}
}
