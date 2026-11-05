package fleet

import (
	"fmt"
	"time"
)

// Lines renders the run as the numbers the two shapes are compared on. Both
// shapes print the same lines in the same order so two runs can be read side by
// side without translating one into the other.
func (r Report) Lines() []string {
	lines := []string{
		fmt.Sprintf("deployments=%d in %s (%d never reached running)",
			r.Deployments, r.Elapsed.Round(time.Millisecond), r.DeploymentTimeouts),
		fmt.Sprintf("nodes that flapped offline and reconnected during the churn: %d", r.Flaps),
		fmt.Sprintf("live nodes whose heartbeat went stale: %d", len(r.Starved)),
		fmt.Sprintf("worst per-deployment event rows=%d (budget %d)", r.WorstEventRows, EventBudget),
	}
	if !r.Reads {
		lines = append(lines, "GET /deployments and GET events: not sampled (the API reader was off)")
	} else {
		lines = append(lines, fmt.Sprintf("GET /deployments p50=%s p99=%s (n=%d); GET events p50=%s p99=%s (n=%d)",
			r.DeploymentLatency.Percentile(0.5).Round(time.Millisecond),
			r.DeploymentLatency.Percentile(0.99).Round(time.Millisecond),
			r.DeploymentLatency.Count(),
			r.EventLatency.Percentile(0.5).Round(time.Millisecond),
			r.EventLatency.Percentile(0.99).Round(time.Millisecond),
			r.EventLatency.Count()))
	}
	if !r.GPUReads {
		lines = append(lines, "GET /gpus: not sampled (the GPU inventory reader was off)")
	} else {
		lines = append(lines, fmt.Sprintf("GET /gpus p50=%s p99=%s (n=%d)",
			r.GPUReadLatency.Percentile(0.5).Round(time.Millisecond),
			r.GPUReadLatency.Percentile(0.99).Round(time.Millisecond),
			r.GPUReadLatency.Count()))
	}
	// Quiesced reads separate list cost from concurrent write pressure.
	lines = append(lines, fmt.Sprintf("GET /deployments with writes quiesced: %s for %d deployments",
		r.QuiescedList.Round(time.Millisecond), r.QuiescedCount))
	if r.SnapshotFailures > 0 {
		lines = append(lines, fmt.Sprintf("liveness reads that failed outright: %d", r.SnapshotFailures))
	}
	return lines
}

// Problems is the shared verdict for both shapes: the same three failures mean
// the same thing whether the fleet shares the server's process or not, so each
// case reports the strings and neither decides for itself what counts.
//
// A brief flap is deliberately not among them. It is reported as a number, and
// what it tracks is understood: the write-deadline rejection that used to close
// live sessions on a healthy link is handled in internal/wsconn, and what is
// left is a saturated host losing and remaking a session. A node that does not
// come back is a different thing, and it is a problem.
func (r Report) Problems(offlineAtEnd []string) []string {
	var problems []string
	if r.Reads {
		problems = append(problems, readProblems("GET /deployments", r.DeploymentLatency, r.ReadFailures, 1)...)
		problems = append(problems, readProblems("GET events", r.EventLatency, nil, 1)...)
	}
	if r.GPUReads {
		problems = append(problems, readProblems("GET /gpus", r.GPUReadLatency, r.GPUReadFailures, 1)...)
	}
	// The failure this whole change exists to prevent: heartbeats queued behind
	// progress writes until the control plane declares a live node dead. It is
	// measured directly, as the age of the last heartbeat of a node the control
	// plane still calls online, so a saturated host that merely drops and
	// remakes a session is not mistaken for it.
	if len(r.Starved) > 0 {
		problems = append(problems, fmt.Sprintf("%d node(s) went %s without a heartbeat while online: %v",
			len(r.Starved), HeartbeatStarvation, r.Starved))
	}
	// A node whose agent is still running must be online when the churn ends.
	if len(offlineAtEnd) > 0 {
		problems = append(problems, fmt.Sprintf("%d node(s) were still offline after the churn while their agent was running: %v",
			len(offlineAtEnd), offlineAtEnd))
	}
	if r.Deployments == 0 {
		problems = append(problems, "no deployment completed; the churn loop measured nothing")
	}
	for _, failure := range r.FailedDeployments {
		problems = append(problems, "deployment failed: "+failure)
	}
	if r.WorstEventRows > EventBudget {
		problems = append(problems, fmt.Sprintf("worst deployment wrote %d event rows (budget %d)",
			r.WorstEventRows, EventBudget))
	}
	// A watch that could not read the fleet reports no flaps and no starvation,
	// which reads exactly like a clean run. Saying so is the difference between
	// "nothing went wrong" and "nothing was observed".
	if r.SnapshotFailures > 0 && r.Flaps == 0 && len(r.Starved) == 0 {
		problems = append(problems, fmt.Sprintf(
			"%d liveness read(s) failed and nothing was observed; the clean flap and heartbeat figures mean nothing",
			r.SnapshotFailures))
	}
	return problems
}
