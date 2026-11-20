package slurm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// sinfo -h -N -o '%N|%T|%G|%E' of a mixed cluster as Slurm 22.05 prints it: a
// node once per partition, GRES with and without type and socket, states
// with the flag suffixes, and a reason that holds spaces and a pipe.
const sinfoMixed = `cn000|idle|gpu:t4:2(S:0)|none
cn000|idle|gpu:t4:2(S:0)|none
cn001|drained*|gpu:t4:1|gres/gpu count reported lower than configured (1 < 2)
cn002|down*|gpu:2|Not responding
cn003|mixed|gpu:a100:8(S:0-1),shard:16|none
cpu001|idle|(null)|none
cpu002|allocated|license:1|none
cn004|draining@|gpu:1|maintenance | until 18:00
cn005|idle|gpu|none
`

func TestParseClusterNodes(t *testing.T) {
	want := []ClusterNode{
		{Name: "cn000", State: "idle", GPUs: 2},
		{Name: "cn001", State: "drained", GPUs: 1, Reason: "gres/gpu count reported lower than configured (1 < 2)"},
		{Name: "cn002", State: "down", GPUs: 2, Reason: "Not responding"},
		{Name: "cn003", State: "mixed", GPUs: 8},
		{Name: "cn004", State: "draining", GPUs: 1, Reason: "maintenance | until 18:00"},
		{Name: "cn005", State: "idle", GPUs: 0},
		// no gpu GRES: listed by name alone, still a node of the cluster
		{Name: "cpu001", ListedOnly: true},
		{Name: "cpu002", ListedOnly: true},
	}
	got := parseClusterNodes([]byte(sinfoMixed))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nodes = %+v\nwant %+v", got, want)
	}
	if got := parseClusterNodes(nil); got == nil || len(got) != 0 {
		t.Fatalf("no output = %#v, want an empty list", got)
	}
	if got := parseClusterNodes([]byte("garbage\n|idle|gpu:1|none\n")); len(got) != 0 {
		t.Fatalf("unusable lines = %+v", got)
	}
}

// The membership digest follows which nodes there are, which have GPUs and
// how many; the state digest follows what is not membership. Neither moves
// with the other.
func TestDigestsFollowTheirOwnFacts(t *testing.T) {
	a := parseClusterNodes([]byte(sinfoMixed))
	b := parseClusterNodes([]byte(sinfoMixed))
	if Digest(a) != Digest(b) || Digest(a) == "" || StateDigest(a) != StateDigest(b) || StateDigest(a) == "" {
		t.Fatal("the digests of one list are not stable")
	}
	b[1].State, b[1].Reason = "idle", ""
	if Digest(a) != Digest(b) {
		t.Fatal("a changed state moved the membership digest: every state change would rewrite the table")
	}
	if StateDigest(a) == StateDigest(b) {
		t.Fatal("a changed state kept the state digest")
	}
	b[1].Reason = "other words"
	b[1].State = a[1].State
	if StateDigest(a) == StateDigest(b) {
		t.Fatal("a changed reason kept the state digest")
	}
	c := parseClusterNodes([]byte(sinfoMixed))
	c[0].GPUs = 4
	if Digest(a) == Digest(c) || StateDigest(a) != StateDigest(c) {
		t.Fatal("a changed GPU count belongs to the membership alone")
	}
	d := parseClusterNodes([]byte(sinfoMixed))
	d[6].ListedOnly = false // a node without GPUs that gained a gpu GRES
	if Digest(a) == Digest(d) {
		t.Fatal("a node that became a GPU node kept the membership digest")
	}
	if Digest(a[:2]) == Digest(a) || Digest(nil) == Digest(a) {
		t.Fatal("a shorter list kept the digest")
	}
	// A node without GPUs is not part of the state digest.
	e := parseClusterNodes([]byte(sinfoMixed))
	e[6].State = "down"
	if StateDigest(a) != StateDigest(e) {
		t.Fatal("the state of a node without GPUs moved the state digest")
	}
}

func TestClusterNodesPassesTheCommandAndItsFailure(t *testing.T) {
	var gotName string
	var gotArgs []string
	nodes, err := ClusterNodes(context.Background(), func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte(sinfoMixed), nil
	})
	// -a: nodes in partitions hidden from the agent's account are nodes too.
	if err != nil || len(nodes) != 8 || gotName != "sinfo" || !reflect.DeepEqual(gotArgs, []string{"-a", "-h", "-N", "-o", "%N|%T|%G|%E"}) {
		t.Fatalf("ClusterNodes = %d nodes, %v; %s %v", len(nodes), err, gotName, gotArgs)
	}
	boom := errors.New("sinfo: error: slurm_load_partitions: Unable to contact slurm controller (connect failure)")
	if _, err := ClusterNodes(context.Background(), func(context.Context, string, ...string) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("a failing sinfo = %v", err)
	}
}

// reportServer is the server's POST /api/v1/slurm/gpu-nodes as it answers:
// 401 for a token that is not a node's, the digest alone answered with
// need_nodes until the list arrives.
type reportServer struct {
	mu       sync.Mutex
	token    string
	stored   string // the membership digest and the state digest it stored
	stateDg  string
	bodies   []map[string]any
	status   int
	settings Settings
}

func (s *reportServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/slurm/gpu-nodes" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		http.Error(w, `{"error":"a Slurm node's token is required"}`, http.StatusUnauthorized)
		return
	}
	if s.status != 0 {
		http.Error(w, "boom", s.status)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.bodies = append(s.bodies, body)
	digest, _ := body["digest"].(string)
	need := false
	switch {
	case digest == s.stored && body["state_digest"] == s.stateDg:
	case body["nodes"] == nil:
		need = true
	default:
		s.stored = digest
		s.stateDg, _ = body["state_digest"].(string)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"need_nodes": need, "settings": s.settings})
}

func TestReporterSendsTheDigestFirstAndTheNodesWhenAsked(t *testing.T) {
	srv := &reportServer{token: "node-token", settings: Settings{SbatchArgs: "-A ai", IdleReturnSeconds: 120, PendingTimeoutSeconds: 900, AccountUser: "svc"}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	var applied []Settings
	r := &Reporter{Server: ts.URL + "/", Token: func() string { return "node-token" }, Command: func(context.Context, string, ...string) ([]byte, error) { return []byte(sinfoMixed), nil },
		Apply: func(s Settings) { applied = append(applied, s) }}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(srv.bodies) != 2 || srv.bodies[0]["nodes"] != nil || srv.bodies[0]["digest"] == "" || srv.bodies[0]["state_digest"] == "" {
		t.Fatalf("first report = %v; want the digest alone, then the nodes", srv.bodies)
	}
	nodes, _ := srv.bodies[1]["nodes"].([]any)
	first, _ := nodes[1].(map[string]any)
	cpu, _ := nodes[6].(map[string]any)
	if len(nodes) != 8 || cpu["name"] != "cpu001" || cpu["listed_only"] != true || first["name"] != "cn001" || first["state"] != "drained" || first["gpus"] != float64(1) || first["reason"] != "gres/gpu count reported lower than configured (1 < 2)" {
		t.Fatalf("nodes sent = %v", nodes)
	}
	// The same list again costs one small request.
	if err := r.Once(context.Background()); err != nil || len(srv.bodies) != 3 {
		t.Fatalf("second report: %v, %d bodies", err, len(srv.bodies))
	}
	want := Settings{SbatchArgs: "-A ai", IdleReturnSeconds: 120, PendingTimeoutSeconds: 900, AccountUser: "svc"}
	if len(applied) != 2 || applied[0] != want || applied[1] != want {
		t.Fatalf("settings applied = %+v", applied)
	}
	// A cluster with no GPU nodes reports an empty list, not none.
	empty := &Reporter{Server: ts.URL, Token: func() string { return "node-token" }, Command: func(context.Context, string, ...string) ([]byte, error) { return nil, nil }}
	if err := empty.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := srv.bodies[len(srv.bodies)-1]
	if nodes, ok := last["nodes"].([]any); !ok || len(nodes) != 0 {
		t.Fatalf("empty cluster sent %v; want nodes: []", last)
	}
}

func TestReporterFailuresAreErrorsAndApplyNothing(t *testing.T) {
	srv := &reportServer{token: "node-token"}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	applied := 0
	run := func(context.Context, string, ...string) ([]byte, error) { return []byte(sinfoMixed), nil }
	apply := func(Settings) { applied++ }
	for name, r := range map[string]*Reporter{
		"wrong token": {Server: ts.URL, Token: func() string { return "other" }, Command: run, Apply: apply},
		"sinfo fails": {Server: ts.URL, Token: func() string { return "node-token" }, Apply: apply, Command: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("sinfo: no controller")
		}},
		"server is down": {Server: "http://127.0.0.1:1", Token: func() string { return "node-token" }, Command: run, Apply: apply},
	} {
		if err := r.Once(context.Background()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	srv.status = http.StatusInternalServerError
	if err := (&Reporter{Server: ts.URL, Token: func() string { return "node-token" }, Command: run, Apply: apply}).Once(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("a failing server = %v", err)
	}
	srv.status = 0
	garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "not json") }))
	defer garbled.Close()
	if err := (&Reporter{Server: garbled.URL, Token: func() string { return "t" }, Command: run, Apply: apply}).Once(context.Background()); err == nil {
		t.Error("an unreadable answer was accepted")
	}
	if applied != 0 {
		t.Fatalf("settings applied %d time(s) after failures", applied)
	}
}

// Run reports soon after the start, then at the interval give or take a
// fifth, and sooner after a failure; it stops with its context.
func TestReporterRunPacesItselfAndRetriesFailures(t *testing.T) {
	srv := &reportServer{token: "t"}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	fail := true
	r := &Reporter{Server: ts.URL, Token: func() string { return "t" }, Interval: 10 * time.Minute,
		Command: func(context.Context, string, ...string) ([]byte, error) {
			if fail {
				fail = false
				return nil, errors.New("sinfo: no controller")
			}
			return []byte(sinfoMixed), nil
		},
		Wait: func(_ context.Context, d time.Duration) bool {
			waits = append(waits, d)
			if len(waits) > 3 {
				cancel()
				return false
			}
			return true
		}}
	r.Run(ctx)
	if len(waits) != 4 {
		t.Fatalf("waits = %v", waits)
	}
	if waits[0] < 20*time.Second || waits[0] >= 60*time.Second {
		t.Errorf("first wait = %v, want 20s to 1m", waits[0])
	}
	if waits[1] != reportRetry {
		t.Errorf("wait after a failure = %v, want %v", waits[1], reportRetry)
	}
	for _, d := range waits[2:] {
		if d < 8*time.Minute || d > 12*time.Minute {
			t.Errorf("wait after a report = %v, want 10 minutes give or take a fifth", d)
		}
	}
	if !sleep(context.Background(), time.Millisecond) {
		t.Error("sleep reported a live context as ended")
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	if sleep(done, time.Hour) {
		t.Error("sleep did not stop with its context")
	}
}

// The reporter asks for its token on every report: one that changed since the
// last (the node enrolled again) is the one the server sees.
func TestReporterSendsTheCurrentToken(t *testing.T) {
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer new-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"need_nodes": false})
	}))
	defer ts.Close()
	token := "old-token"
	r := &Reporter{Server: ts.URL, Token: func() string { return token }, Command: func(context.Context, string, ...string) ([]byte, error) { return []byte(sinfoMixed), nil }}
	if err := r.Once(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("with the old token: %v", err)
	}
	token = "new-token"
	if err := r.Once(context.Background()); err != nil {
		t.Fatalf("with the new token: %v", err)
	}
	if strings.Join(seen, "|") != "Bearer old-token|Bearer new-token" {
		t.Fatalf("authorization headers = %v", seen)
	}
}

// A state that changes sends the state digest alone again, and the nodes are
// sent only when the server asks; the membership digest stays the same.
func TestReporterStateChangesKeepTheMembershipDigest(t *testing.T) {
	srv := &reportServer{token: "node-token"}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	out := sinfoMixed
	r := &Reporter{Server: ts.URL, Token: func() string { return "node-token" }, Command: func(context.Context, string, ...string) ([]byte, error) { return []byte(out), nil }}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	out = strings.Replace(sinfoMixed, "cn000|idle|gpu:t4:2(S:0)|none\ncn000|idle|gpu:t4:2(S:0)|none", "cn000|allocated|gpu:t4:2(S:0)|none\ncn000|allocated|gpu:t4:2(S:0)|none", 1)
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(srv.bodies) != 4 {
		t.Fatalf("bodies = %d, want the first report (2) and a state change (2)", len(srv.bodies))
	}
	if srv.bodies[2]["digest"] != srv.bodies[0]["digest"] || srv.bodies[2]["state_digest"] == srv.bodies[0]["state_digest"] || srv.bodies[3]["nodes"] == nil {
		t.Fatalf("reports = %v", srv.bodies)
	}
}
