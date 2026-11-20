package slurm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// enrollServer is the server's POST /api/v1/slurm/enroll: a token it knows
// gets the node, another Slurm cluster, an unknown token and a held node get
// the coded 403 the real handler writes.
type enrollServer struct {
	token, cluster string
	reply          string
	status         int
	got            []map[string]string
}

func (s *enrollServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/slurm/enroll" {
		http.NotFound(w, r)
		return
	}
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.got = append(s.got, body)
	if s.status != 0 {
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.reply))
		return
	}
	refuse := func(reason string) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"refused","code":"SLURM_ENROLL_REFUSED","params":{"reason":"` + reason + `"}}`))
	}
	switch {
	case body["token"] != s.token:
		refuse("unknown_token")
	case body["slurm_cluster_name"] != s.cluster:
		refuse("other_cluster")
	default:
		_, _ = w.Write([]byte(`{"node_uuid":"uuid-` + body["node_name"] + `","node_token":"tok-` + body["node_name"] + `","settings":{"sbatch_args":"-A ai","idle_return_seconds":120,"pending_timeout_seconds":900,"account_user":"svc"}}`))
	}
}

func TestEnrollSavesTheIdentityAndAsksWithTheNodesFacts(t *testing.T) {
	srv := &enrollServer{token: "enroll", cluster: "demo"}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	id, err := Enroll(context.Background(), nil, ts.URL+"/", "enroll", "demo", "gpu-1", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{NodeUUID: "uuid-gpu-1", NodeToken: "tok-gpu-1", Entry: EntryOf("enroll"),
		Settings: Settings{SbatchArgs: "-A ai", IdleReturnSeconds: 120, PendingTimeoutSeconds: 900, AccountUser: "svc"}}
	if id != want {
		t.Fatalf("identity = %+v, want %+v", id, want)
	}
	if got := srv.got[0]; got["token"] != "enroll" || got["slurm_cluster_name"] != "demo" || got["node_name"] != "gpu-1" || len(got) != 3 {
		t.Fatalf("request = %v", got)
	}
	info, err := os.Stat(filepath.Join(dir, IdentityFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity file = %v, %v; want 0600", info, err)
	}
	if saved := ReadIdentity(dir, "enroll"); saved != id {
		t.Fatalf("saved = %+v", saved)
	}
	if other := ReadIdentity(dir, "another-entry"); other != (Identity{}) {
		t.Fatalf("another entry's token read %+v as its own identity", other)
	}
}

// The server answers the same node every time, so a start asks again (a
// changed setting arrives) and rewrites the file only when something changed.
func TestEnrollAsksOnEveryStartAndKeepsTheFileWhenNothingChanged(t *testing.T) {
	srv := &enrollServer{token: "enroll", cluster: "demo"}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	if _, err := Enroll(context.Background(), nil, ts.URL, "enroll", "demo", "gpu-1", dir, true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, IdentityFile)
	old := filepath.Join(dir, "marker")
	if err := os.Rename(path, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(old, path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if _, err := Enroll(context.Background(), nil, ts.URL, "enroll", "demo", "gpu-1", dir, true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || len(srv.got) != 2 {
		t.Fatalf("the unchanged identity was rewritten (same file %v) or not asked for (%d requests)", os.SameFile(before, after), len(srv.got))
	}
}

func TestEnrollRefusalsAndTheSavedIdentity(t *testing.T) {
	srv := &enrollServer{token: "enroll", cluster: "demo"}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	saved, err := Enroll(context.Background(), nil, ts.URL, "enroll", "demo", "gpu-1", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	// A revoked or unknown token stops new nodes, not the ones that enrolled:
	// the saved identity keeps running.
	id, err := Enroll(context.Background(), nil, ts.URL, "revoked", "demo", "gpu-1", dir, true)
	if err == nil {
		t.Fatalf("an entry other than the one that issued the identity was honoured: %+v", id)
	}
	srv.token = "revoked-now"
	if id, err := Enroll(context.Background(), nil, ts.URL, "enroll", "demo", "gpu-1", dir, true); err != nil || id != saved {
		t.Fatalf("revoked token with a saved identity = %+v, %v; want the saved one", id, err)
	}
	// A refusal about this node's own identity is never papered over.
	srv.token = "enroll"
	var refusal *EnrollRefusal
	if _, err := Enroll(context.Background(), nil, ts.URL, "enroll", "other-cluster", "gpu-1", dir, true); !errors.As(err, &refusal) || refusal.Reason != "other_cluster" {
		t.Fatalf("other cluster = %v", err)
	}
	// fallback=false (after a refusal) refuses even when the server is silent.
	srv.status, srv.reply = http.StatusBadGateway, "bad gateway"
	if _, err := Enroll(context.Background(), nil, ts.URL, "enroll", "demo", "gpu-1", dir, false); err == nil {
		t.Fatal("an outage hid an earlier refusal behind the saved identity")
	}
	if id, err := Enroll(context.Background(), nil, ts.URL, "enroll", "demo", "gpu-1", dir, true); err != nil || id != saved {
		t.Fatalf("an outage with a saved identity = %+v, %v", id, err)
	}
	// A node that never enrolled needs the server to say yes.
	if _, err := Enroll(context.Background(), nil, ts.URL, "enroll", "demo", "gpu-2", t.TempDir(), true); err == nil {
		t.Fatal("a node that never enrolled started during an outage")
	}
}

func TestEnrollNodeTakenAndForeignRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		status      int
		reason      string
		entry       string
		refusal     bool
		falls       bool
	}{
		{"node taken", `{"error":"x","code":"SLURM_ENROLL_REFUSED","params":{"reason":"node_taken","entry":"hpc-a"}}`, 403, "node_taken", "hpc-a", true, false},
		{"unknown token", `{"error":"x","code":"SLURM_ENROLL_REFUSED","params":{"reason":"unknown_token"}}`, 403, "unknown_token", "", true, true},
		{"no reason (older server)", `{"error":"x","code":"SLURM_ENROLL_REFUSED"}`, 403, "", "", true, true},
		// A proxy's own 403 says nothing about this node's identity: a plain failure.
		{"proxy 403", `<html>Forbidden</html>`, 403, "", "", false, false},
		{"another product error", `{"code":"KUBE_ENROLL_REFUSED","params":{"reason":"other_cluster"}}`, 403, "", "", false, false},
		{"server error", `oops`, 500, "", "", false, false},
	} {
		srv := &enrollServer{status: tc.status, reply: tc.reply}
		ts := httptest.NewServer(srv)
		saved := t.TempDir()
		_ = writeIdentity(filepath.Join(saved, IdentityFile), Identity{NodeUUID: "u", NodeToken: "t", Entry: EntryOf("enroll")})
		id, err := Enroll(context.Background(), nil, ts.URL, "enroll", "demo", "gpu-1", saved, true)
		var refusal *EnrollRefusal
		isRefusal := errors.As(err, &refusal)
		// With an identity of this entry saved, a refusal that does not touch
		// the node's identity is not an error: the node runs on what it has.
		if want := tc.refusal && !tc.falls; isRefusal != want {
			t.Errorf("%s: refusal = %v (%v), want %v", tc.name, isRefusal, err, want)
		}
		if isRefusal && (refusal.Reason != tc.reason || refusal.Entry != tc.entry) {
			t.Errorf("%s: reason %q entry %q", tc.name, refusal.Reason, refusal.Entry)
		}
		// What may fall back to the saved identity: everything that is not a
		// refusal of this node's identity.
		wantSaved := !tc.refusal || tc.falls
		if gotSaved := err == nil && id.NodeToken == "t"; gotSaved != wantSaved {
			t.Errorf("%s: fell back to the saved identity = %v, want %v (err %v)", tc.name, gotSaved, wantSaved, err)
		}
		ts.Close()
	}
}

func TestEnrollRejectsAnUnusableAnswer(t *testing.T) {
	srv := &enrollServer{status: 200, reply: `{"node_uuid":"u"}`}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	if _, err := Enroll(context.Background(), nil, ts.URL, "enroll", "demo", "gpu-1", t.TempDir(), true); err == nil || !strings.Contains(err.Error(), "unexpected answer") {
		t.Fatalf("an answer with no node token = %v", err)
	}
}

func TestClusterNameFromScontrolShowConfig(t *testing.T) {
	config := "Configuration data as of 2026-11-17T08:00:00\nAccountingStorageType   = accounting_storage/none\nClusterName             = ehpc-sz-demo\nSlurmctldHost[0]        = manager\n"
	got, err := ClusterName(context.Background(), func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "scontrol" || strings.Join(args, " ") != "show config" {
			t.Fatalf("command %s %v", name, args)
		}
		return []byte(config), nil
	})
	if err != nil || got != "ehpc-sz-demo" {
		t.Fatalf("ClusterName = %q, %v", got, err)
	}
	for name, out := range map[string]string{"none": "SlurmUser = slurm\n", "null": "ClusterName = (null)\n", "empty": "ClusterName =\n"} {
		if _, err := ClusterName(context.Background(), func(context.Context, string, ...string) ([]byte, error) { return []byte(out), nil }); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	boom := errors.New("scontrol: error: slurm_load_ctl_conf")
	if _, err := ClusterName(context.Background(), func(context.Context, string, ...string) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("a failing scontrol = %v", err)
	}
}
