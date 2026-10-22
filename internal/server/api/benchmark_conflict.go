package api

import (
	"fmt"
	"net/http"
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Active-run admission. Creating a run never queues behind another one: the
// executor and the desired-state push both handle every active run
// independently, so concurrency is a measurement decision rather than a
// protocol limitation. What the checks below produce is therefore an advisory
// list of the runs this new one would contend with, which the caller turns into
// a refusal the user may override with allow_conflict.
const (
	// Same load origin: two runs generating load from one place distort each
	// other's timings, and "server" is a load origin like any other.
	benchmarkConflictLoadOrigin = "load_origin"
	// Same managed target: two runs measuring one deployment share its engine.
	benchmarkConflictDeployment = "deployment"
	// Same target node, different deployments: the runs measure different
	// engines, but those engines share the node's GPUs and host resources.
	// This is the case the older checks could not see at all.
	benchmarkConflictTargetNode = "target_node"
	// A server-origin execution already in flight. It is also a load_origin
	// conflict, but the in-process gate is the only thing that closes the window
	// before the first status of that run is persisted.
	benchmarkConflictServerActive = "server_active"
)

// benchmarkConflict is one active run a new run would contend with.
type benchmarkConflict struct {
	Kind    string
	RunID   uint
	RunName string
}

// benchmarkConflicts lists the active runs this request would contend with. The
// database is authoritative for every kind; the in-process server gate is
// evaluated first so a server-origin request reports the same code it always
// has, and targetNodeID is the node hosting the new run's target leader (0 when
// the target is external, which has no leader to share).
func (a *apiHandlers) benchmarkConflicts(loadOrigin, targetKind string, deploymentID, targetNodeID uint) []benchmarkConflict {
	var conflicts []benchmarkConflict
	if loadOrigin == "server" {
		if conflict, ok := a.activeServerBenchmark(); ok {
			conflicts = append(conflicts, conflict)
		}
	}
	runs, err := a.d.Store.ListBenchmarkRuns()
	if err != nil {
		return conflicts
	}
	for i := range runs {
		run := runs[i]
		if run.State != "pending" && run.State != "running" {
			continue
		}
		kind := ""
		switch {
		case run.LoadOrigin == loadOrigin:
			kind = benchmarkConflictLoadOrigin
		case targetKind == "instance" && deploymentID != 0 && run.DeploymentID == deploymentID:
			kind = benchmarkConflictDeployment
		case targetNodeID != 0 && a.benchmarkTargetNodeID(run) == targetNodeID:
			kind = benchmarkConflictTargetNode
		}
		if kind == "" {
			continue
		}
		conflicts = append(conflicts, benchmarkConflict{Kind: kind, RunID: run.ID, RunName: run.Name})
	}
	return conflicts
}

// activeServerBenchmark reports a server-origin execution running in this
// process, oldest first so repeated attempts name the same run.
func (a *apiHandlers) activeServerBenchmark() (benchmarkConflict, bool) {
	a.benchmarkMu.Lock()
	ids := make([]uint, 0, len(a.benchmarkExecutions))
	for id := range a.benchmarkExecutions {
		ids = append(ids, id)
	}
	a.benchmarkMu.Unlock()
	if len(ids) == 0 {
		return benchmarkConflict{}, false
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	id := ids[0]
	conflict := benchmarkConflict{Kind: benchmarkConflictServerActive, RunID: id}
	if run, err := a.d.Store.GetBenchmarkRun(id); err == nil {
		conflict.RunName = run.Name
	}
	return conflict, true
}

// benchmarkTargetNodeID is the node hosting a run's managed target leader, or 0
// for an external target, a deployment that no longer exists, or a run created
// before the leader was recorded. The run stores the leader it resolved at
// creation, which is the instance it is actually measuring.
func (a *apiHandlers) benchmarkTargetNodeID(run store.BenchmarkRun) uint {
	if run.TargetKind != "instance" || run.DeploymentID == 0 {
		return 0
	}
	if run.InstanceUUID != "" {
		if inst, err := a.d.Store.InstanceByUUID(run.InstanceUUID); err == nil {
			return inst.NodeID
		}
	}
	instances, err := a.d.Store.InstancesForDeployment(run.DeploymentID)
	if err != nil {
		return 0
	}
	for i := range instances {
		if instances[i].NodeRank == 0 {
			return instances[i].NodeID
		}
	}
	return 0
}

// writeBenchmarkConflict refuses a conflicting create. The params carry the run
// identity and the kind of overlap so the client can compose the sentence it
// shows; the English fallback stays readable for clients that do not.
func writeBenchmarkConflict(w http.ResponseWriter, conflicts []benchmarkConflict) {
	first := conflicts[0]
	if first.Kind == benchmarkConflictServerActive {
		writeProductError(w, http.StatusConflict, ErrorBenchmarkServerActive,
			fmt.Sprintf("a server benchmark is already running (run %d)", first.RunID),
			map[string]any{"run_id": first.RunID, "run_name": first.RunName})
		return
	}
	writeProductError(w, http.StatusConflict, ErrorBenchmarkRunConflict,
		fmt.Sprintf("benchmark conflicts with running run %d", first.RunID),
		map[string]any{"run_id": first.RunID, "run_name": first.RunName, "kind": first.Kind, "count": len(conflicts)})
}

// recordBenchmarkConflictWarning marks an admitted override on the run, so the
// decision survives the dialog that made it.
func (a *apiHandlers) recordBenchmarkConflictWarning(runID uint, conflicts []benchmarkConflict) {
	if len(conflicts) == 0 {
		return
	}
	if err := a.d.Store.AddBenchmarkWarning(runID, proto.BenchmarkWarningConflictOverridden); err != nil && a.d.Log != nil {
		a.d.Log.Warn("record benchmark conflict warning failed", "run", runID, "err", err)
	}
}
