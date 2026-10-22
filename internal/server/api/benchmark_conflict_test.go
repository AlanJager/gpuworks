package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// benchmarkAdmission is the smallest cluster an admission decision needs: a
// store, the tokens a create request carries, and handlers that own the
// in-process server gate.
type benchmarkAdmission struct {
	store    *store.Store
	handlers *apiHandlers
	token    string
}

func newBenchmarkAdmission(t *testing.T, dsn string) *benchmarkAdmission {
	t.Helper()
	st, err := store.Open("file:"+dsn+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, err := auth.NewTokenSigner("benchmark-"+dsn, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("benchmark-" + dsn)
	if err != nil {
		t.Fatal(err)
	}
	return &benchmarkAdmission{
		store:    st,
		token:    token,
		handlers: &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, AuthSigner: signer, AISecretbox: box, BenchmarkArtifactDir: t.TempDir()}, benchmarkExecutions: map[uint]*benchmarkExecution{}},
	}
}

// managedTarget seeds a node hosting the running leader of an active
// deployment, which is what makes an instance-target create resolvable.
func (a *benchmarkAdmission) managedTarget(t *testing.T, name string) (*store.Deployment, *store.Node) {
	t.Helper()
	node, err := a.store.RegisterNode(name, store.HashToken("token-"+name), name)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := a.store.CreateDeployment(&store.Deployment{Name: name, ServedName: name + "-served", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.CreateInstance(&store.Instance{UUID: name + "-leader", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 31000, GPUIndexes: `[0]`}); err != nil {
		t.Fatal(err)
	}
	return dep, node
}

// activeRun records a run directly: the admission decision reads the persisted
// state, not how the run got there.
func (a *benchmarkAdmission) activeRun(t *testing.T, run store.BenchmarkRun) *store.BenchmarkRun {
	t.Helper()
	if run.State == "" {
		run.State = "running"
	}
	created, err := a.store.CreateBenchmarkRun(&run)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (a *benchmarkAdmission) create(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1/api/v1/benchmarks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.handlers.createBenchmark(rec, req)
	return rec
}

// conflictBody measures from the target's node: node-origin runs are the ones
// that compete for a node's load generator.
func conflictBody(deploymentID uint, origin string, allowConflict bool) string {
	return fmt.Sprintf(`{"name":"new-run","target_kind":"instance","deployment_id":%d,"load_origin":%q,"dataset":{"kind":"random","input_tokens":8,"output_tokens":4},"mode":"grid","search":{"concurrency":[1],"isl":[8],"osl":[4],"requests_per_point_multiplier":1},"allow_conflict":%t}`, deploymentID, origin, allowConflict)
}

func TestBenchmarkAdmissionFlagsEachKindOfOverlap(t *testing.T) {
	a := newBenchmarkAdmission(t, "benchmark_admission_kinds")
	firstDeployment, sharedNode := a.managedTarget(t, "node-a")
	// A second deployment on the same node is the gap the older checks had:
	// nothing about the load origin or the target deployment matches.
	secondDeployment, err := a.store.CreateDeployment(&store.Deployment{Name: "second", ServedName: "second-served", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.CreateInstance(&store.Instance{UUID: "second-leader", DeploymentID: secondDeployment.ID, NodeID: sharedNode.ID, State: proto.InstRunning, APIPort: 31001}); err != nil {
		t.Fatal(err)
	}
	// A run elsewhere, for the negative cases.
	_, otherNode := a.managedTarget(t, "node-b")

	// One kind per active run, most specific first: a run that shares both the
	// load origin and the target reports the load origin, exactly as the
	// pre-existing check did.
	cases := []struct {
		name     string
		active   store.BenchmarkRun
		wantKind string
	}{
		{
			name:     "same managed target",
			active:   store.BenchmarkRun{TargetKind: "instance", DeploymentID: firstDeployment.ID, InstanceUUID: "node-a-leader", LoadOrigin: "somewhere-else"},
			wantKind: benchmarkConflictDeployment,
		},
		{
			name:     "same load origin on another deployment",
			active:   store.BenchmarkRun{TargetKind: "instance", DeploymentID: secondDeployment.ID, InstanceUUID: "second-leader", LoadOrigin: sharedNode.UUID},
			wantKind: benchmarkConflictLoadOrigin,
		},
		{
			name:     "same target node under different deployments",
			active:   store.BenchmarkRun{TargetKind: "instance", DeploymentID: secondDeployment.ID, InstanceUUID: "second-leader", LoadOrigin: "server"},
			wantKind: benchmarkConflictTargetNode,
		},
		{
			name:     "other node and other origin",
			active:   store.BenchmarkRun{TargetKind: "instance", InstanceUUID: "", LoadOrigin: otherNode.UUID},
			wantKind: "",
		},
		{
			name:     "an external target has no node to share",
			active:   store.BenchmarkRun{TargetKind: "external", LoadOrigin: otherNode.UUID},
			wantKind: "",
		},
		{
			name:     "terminal runs never conflict",
			active:   store.BenchmarkRun{TargetKind: "instance", DeploymentID: firstDeployment.ID, InstanceUUID: "node-a-leader", LoadOrigin: "server", State: "done"},
			wantKind: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stale, err := a.store.ListBenchmarkRuns()
			if err != nil {
				t.Fatal(err)
			}
			for i := range stale {
				if stale[i].State == "pending" || stale[i].State == "running" {
					if err := a.store.CancelBenchmarkRun(stale[i].ID, "test cleanup"); err != nil {
						t.Fatal(err)
					}
				}
			}
			created := a.activeRun(t, tc.active)
			// The new run measures the first deployment from its own node.
			conflicts := a.handlers.benchmarkConflicts(sharedNode.UUID, "instance", firstDeployment.ID, sharedNode.ID)
			kind := ""
			for _, conflict := range conflicts {
				if conflict.RunID == created.ID {
					kind = conflict.Kind
				}
			}
			if kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q (conflicts=%+v)", kind, tc.wantKind, conflicts)
			}
		})
	}
}

func TestBenchmarkAdmissionRefusesThenAdmitsOnRequest(t *testing.T) {
	a := newBenchmarkAdmission(t, "benchmark_admission_override")
	deployment, node := a.managedTarget(t, "override-node")
	a.activeRun(t, store.BenchmarkRun{Name: "already-running", TargetKind: "instance", DeploymentID: deployment.ID, InstanceUUID: "override-node-leader", LoadOrigin: node.UUID})

	refused := a.create(t, conflictBody(deployment.ID, node.UUID, false))
	if refused.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", refused.Code, refused.Body.String())
	}
	var body struct {
		Error  string         `json:"error"`
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(refused.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != ErrorBenchmarkRunConflict {
		t.Fatalf("code=%s body=%s", body.Code, refused.Body.String())
	}
	// The client composes the sentence it shows, so every fact it needs has to
	// be on the wire: which run, what it overlaps, and how many.
	if body.Params["run_id"] != float64(1) || body.Params["run_name"] != "already-running" || body.Params["kind"] != benchmarkConflictLoadOrigin || body.Params["count"] != float64(1) {
		t.Fatalf("params=%v", body.Params)
	}
	if runs, err := a.store.ListBenchmarkRuns(); err != nil || len(runs) != 1 {
		t.Fatalf("a refused create must persist nothing: runs=%d err=%v", len(runs), err)
	}

	admitted := a.create(t, conflictBody(deployment.ID, node.UUID, true))
	if admitted.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", admitted.Code, admitted.Body.String())
	}
	var created benchmarkDTO
	if err := json.Unmarshal(admitted.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	persisted, err := a.store.GetBenchmarkRun(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var warnings []string
	if err := json.Unmarshal([]byte(persisted.Warnings), &warnings); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0] != proto.BenchmarkWarningConflictOverridden {
		t.Fatalf("warnings=%v", warnings)
	}
	// An unrelated create still carries no warning: the record marks the
	// override, not the run.
	clean, err := a.store.CreateBenchmarkRun(&store.BenchmarkRun{Name: "clean", TargetKind: "external", LoadOrigin: "elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	if clean.Warnings != "" {
		t.Fatalf("unexpected warnings on an unrelated run: %q", clean.Warnings)
	}
}

func TestBenchmarkAdmissionServerGateNamesTheRunningRun(t *testing.T) {
	a := newBenchmarkAdmission(t, "benchmark_admission_server")
	active, err := a.store.CreateBenchmarkRun(&store.BenchmarkRun{Name: "server-run", TargetKind: "external", LoadOrigin: "server"})
	if err != nil {
		t.Fatal(err)
	}
	a.handlers.benchmarkMu.Lock()
	a.handlers.benchmarkExecutions[active.ID] = &benchmarkExecution{cancel: func() {}, done: make(chan struct{})}
	a.handlers.benchmarkMu.Unlock()

	conflict, ok := a.handlers.activeServerBenchmark()
	if !ok || conflict.RunID != active.ID || conflict.RunName != "server-run" || conflict.Kind != benchmarkConflictServerActive {
		t.Fatalf("conflict=%+v ok=%t", conflict, ok)
	}

	// The gate is what a second server-origin request hits first, and it keeps
	// its own error code while gaining the run identity.
	rec := httptest.NewRecorder()
	writeBenchmarkConflict(rec, []benchmarkConflict{conflict})
	var body struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict || body.Code != ErrorBenchmarkServerActive {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body.Params["run_id"] != float64(active.ID) || body.Params["run_name"] != "server-run" || len(body.Params) != 2 {
		t.Fatalf("params=%v", body.Params)
	}
}
