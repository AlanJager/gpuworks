package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// PullTranscript is a compact stand-in for one node's pull output: enough lines
// to exercise the agent's egress limiter, replayed by every redeploying node at
// once. The exact dialect is covered by the amplification case; here the point
// is the aggregate rate. Both shapes replay the same lines so the per-node work
// is identical.
func PullTranscript(lines int) []string {
	out := make([]string, 0, lines)
	total := 1 << 30
	for i := 1; i <= lines; i++ {
		done := int64(total) * int64(i) / int64(lines)
		out = append(out, fmt.Sprintf("a2abf6c4d29d: Downloading %d B/%d B", done, total))
	}
	return out
}

// EventBudget is the per-deployment event-row ceiling this whole change exists
// to hold: a deployment is a handful of milestones, not a transcript.
const EventBudget = 20

// EnvInt reads a positive integer from the environment, falling back when it is
// unset or unusable, so both shapes scale the same way from the same variables.
func EnvInt(name string, fallback int) int {
	if raw := os.Getenv(name); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			return value
		}
	}
	return fallback
}

// ReadsEnabled reports whether the concurrent API reader runs.
// GPUWORKS_FLEET_READS=0 drops it, which separates what the churn costs the
// control plane from what a polling browser adds to it.
func ReadsEnabled() bool { return os.Getenv("GPUWORKS_FLEET_READS") != "0" }

// Churn is the measured workload: hold a fleet connected while some of it
// redeploys in a loop, and record what that costs the control plane.
type Churn struct {
	Client  *Client
	ModelID uint
	// ChurnNodes are the node UUIDs that redeploy. Every other node in the
	// fleet stays connected and idle, which is what a real fleet looks like.
	ChurnNodes []string
	Rounds     int
	// Reads runs the concurrent API reader.
	Reads bool
	// Watch observes node liveness for the duration. Its Snapshot must be set.
	Watch *NodeWatch
	// Log receives progress lines; nil discards them.
	Log func(format string, args ...any)
}

// Report is what one run of the workload measured.
type Report struct {
	Deployments        int
	Elapsed            time.Duration
	Flaps              int
	Starved            []string
	SnapshotFailures   int
	WorstEventRows     int
	DeploymentLatency  *Latencies
	EventLatency       *Latencies
	QuiescedList       time.Duration
	QuiescedCount      int
	FailedDeployments  []string
	DeploymentTimeouts int
	Reads              bool
	ReadFailures       []string
}

// Run drives the churn and returns what it measured. Errors that stop the
// workload outright are returned; per-deployment failures are collected in the
// report, because a run that half worked is a result worth seeing.
func (c Churn) Run() (Report, error) {
	if c.Client == nil || c.Watch == nil || c.Watch.Snapshot == nil {
		return Report{}, fmt.Errorf("churn needs a client and a node watch")
	}
	logf := c.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	report := Report{DeploymentLatency: &Latencies{}, EventLatency: &Latencies{}, Reads: c.Reads}

	watchStop, watchDone := make(chan struct{}), make(chan struct{})
	go c.Watch.Run(watchStop, watchDone)

	readStop, readDone := make(chan struct{}), make(chan struct{})
	if c.Reads {
		go c.pollReadEndpoints(&report, readStop, readDone)
	} else {
		close(readDone)
	}

	instances := &instanceWatch{byDep: map[uint]instanceView{}}
	instanceStop, instanceDone := make(chan struct{}), make(chan struct{})
	go instances.run(c.Client, instanceStop, instanceDone)

	var eventRows sync.Map
	var failures sync.Map
	start := time.Now()
	for round := 0; round < c.Rounds; round++ {
		created := make([]uint, len(c.ChurnNodes))
		var wg sync.WaitGroup
		for i, uuid := range c.ChurnNodes {
			wg.Add(1)
			go func(index int, nodeUUID string, round int) {
				defer wg.Done()
				created[index] = c.runOneDeployment(round, nodeUUID, instances, &eventRows, &failures)
			}(i, uuid, round)
		}
		wg.Wait()
		logf("round %d: %d deployment attempts finished", round+1, len(c.ChurnNodes))
		if round == c.Rounds-1 {
			// Everything is running and nothing is writing: the deployment list
			// at its largest, read with no contention at all. It is what
			// separates the list endpoint's own cost from the churn's writes.
			report.QuiescedCount, report.QuiescedList = c.quiescedList(5)
		}
		for _, id := range created {
			wg.Add(1)
			go func(id uint) {
				defer wg.Done()
				if id != 0 {
					_ = c.Client.Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(id), 10))
				}
			}(id)
		}
		wg.Wait()
	}
	report.Elapsed = time.Since(start)

	close(instanceStop)
	<-instanceDone
	close(readStop)
	<-readDone
	close(watchStop)
	<-watchDone

	eventRows.Range(func(_, value any) bool {
		report.Deployments++
		if count := value.(int); count > report.WorstEventRows {
			report.WorstEventRows = count
		}
		return true
	})
	failures.Range(func(_, value any) bool {
		report.FailedDeployments = append(report.FailedDeployments, value.(string))
		return true
	})
	sort.Strings(report.FailedDeployments)
	report.DeploymentTimeouts = len(report.FailedDeployments)
	report.Flaps = c.Watch.Flaps()
	report.Starved = c.Watch.Starved()
	report.SnapshotFailures = c.Watch.SnapshotFailures()
	return report, nil
}

type createdDeployment struct {
	ID uint `json:"id"`
}

type instanceView struct {
	UUID         string `json:"uuid"`
	DeploymentID uint   `json:"deployment_id"`
	State        string `json:"state"`
	Phase        string `json:"phase"`
	Message      string `json:"message"`
}

type deploymentView struct {
	ID uint `json:"id"`
}

// instancePollInterval is how often the whole fleet's instances are re-read.
// One read for everything, not one per deployment: five hundred deployments
// each polling their own detail endpoint would put the case's own reads ahead
// of the workload it is measuring, because that endpoint issues a handful of
// queries per deployment against a pool of one connection.
const instancePollInterval = time.Second

// runOneDeployment creates one deployment, waits for it to run, and records how
// many event rows it took to get there. It returns the deployment id so the
// caller can delete it once the whole round is up.
func (c Churn) runOneDeployment(round int, nodeUUID string, instances *instanceWatch, eventRows, failures *sync.Map) uint {
	attempt := fmt.Sprintf("round %d on %s", round+1, nodeUUID)
	body, _ := json.Marshal(map[string]any{
		"model_id": c.ModelID, "node_uuid": nodeUUID, "served_name": fmt.Sprintf("fleet-%d-%s", round, nodeUUID),
		"quick_params": map[string]any{"max_model_len": 512},
	})
	var created createdDeployment
	if err := c.Client.PostJSON("/api/v1/deployments", body, &created); err != nil {
		failures.Store(attempt, "create, "+attempt+": "+err.Error())
		return 0
	}
	if created.ID == 0 {
		failures.Store(attempt, "create, "+attempt+": no id returned")
		return 0
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if view, ok := instances.get(created.ID); ok && view.State == "running" {
			var events apiwire.CursorPage[json.RawMessage]
			path := "/api/v1/deployments/" + strconv.FormatUint(uint64(created.ID), 10) + "/events?limit=200"
			if err := c.Client.GetJSON(path, &events); err == nil {
				eventRows.Store(created.ID, len(events.Items))
			} else {
				eventRows.Store(created.ID, 0)
			}
			return created.ID
		}
		time.Sleep(instancePollInterval)
	}
	last := "no instance was ever listed for it"
	if view, ok := instances.get(created.ID); ok {
		last = fmt.Sprintf("%s phase=%s msg=%q", view.State, view.Phase, view.Message)
	}
	failures.Store(attempt, fmt.Sprintf("deployment %d (%s) never reached running: %s", created.ID, attempt, last))
	return created.ID
}

// instanceWatch is the one reader of the fleet's instances. Every waiting
// deployment reads its answer out of the same snapshot, so the number of
// deployments in flight does not change how hard the case leans on the control
// plane - only the workload does.
type instanceWatch struct {
	mu    sync.RWMutex
	byDep map[uint]instanceView
}

func (w *instanceWatch) run(client *Client, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		var rows apiwire.List[instanceView]
		if err := client.GetJSON("/api/v1/instances", &rows); err == nil {
			next := make(map[uint]instanceView, len(rows.Items))
			for _, row := range rows.Items {
				// A deployment has one instance in this workload; if that ever
				// changes, the running one is the one being waited for.
				if existing, ok := next[row.DeploymentID]; ok && existing.State == "running" {
					continue
				}
				next[row.DeploymentID] = row
			}
			w.mu.Lock()
			w.byDep = next
			w.mu.Unlock()
		}
		select {
		case <-stop:
			return
		case <-time.After(instancePollInterval):
		}
	}
}

func (w *instanceWatch) get(deploymentID uint) (instanceView, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	view, ok := w.byDep[deploymentID]
	return view, ok
}

// pollReadEndpoints is the browser an operator leaves open: the deployment list
// and one deployment's events, five times a second between them.
func (c Churn) pollReadEndpoints(report *Report, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		case <-time.After(200 * time.Millisecond):
		}
		if took, err := c.Client.Discard("/api/v1/deployments"); err == nil {
			report.DeploymentLatency.Add(took)
		} else {
			report.ReadFailures = append(report.ReadFailures, err.Error())
		}
		if took, err := c.Client.Discard("/api/v1/deployments/1/events?limit=50"); err == nil {
			report.EventLatency.Add(took)
		} else {
			report.ReadFailures = append(report.ReadFailures, err.Error())
		}
	}
}

// quiescedList returns how many deployments the list held and the median of
// several sequential reads taken with nothing else writing.
func (c Churn) quiescedList(samples int) (int, time.Duration) {
	var all apiwire.List[deploymentView]
	_ = c.Client.GetJSON("/api/v1/deployments", &all)
	measured := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		took, err := c.Client.Discard("/api/v1/deployments")
		if err != nil {
			continue
		}
		measured = append(measured, took)
	}
	if len(measured) == 0 {
		return len(all.Items), 0
	}
	sort.Slice(measured, func(i, j int) bool { return measured[i] < measured[j] })
	return len(all.Items), measured[len(measured)/2]
}

// ListNodes reads the fleet's liveness the way an operator's dashboard does.
func ListNodes(client *Client) ([]NodeState, error) {
	var rows apiwire.List[NodeState]
	if err := client.GetJSON("/api/v1/nodes", &rows); err != nil {
		return nil, err
	}
	return rows.Items, nil
}

// WaitFleetOnline polls until every node is online, or reports how far it got.
// One poll for the whole fleet: waiting per node in turn would dominate the run
// and measure the harness rather than the server.
func WaitFleetOnline(client *Client, want int, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	online := 0
	for time.Now().Before(deadline) {
		rows, err := ListNodes(client)
		if err == nil {
			online = 0
			for _, row := range rows {
				if row.State == NodeStateOnline {
					online++
				}
			}
			if online >= want {
				return online, nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return online, fmt.Errorf("only %d of %d nodes came online within %s", online, want, timeout)
}

// ReconnectSettle is how long a node that dropped at the very end of the churn
// is given to come back. An agent whose session lasted more than a minute
// retries after about a second and backs off to at most thirty (the backoff in
// internal/agent/app/agent.go), so a minute is well clear of a healthy
// reconnect and still short enough that a node which is really gone is caught.
const ReconnectSettle = time.Minute

// OfflineNodes lists the nodes the control plane still calls offline once the
// fleet has had a chance to settle, which means a node whose agent is running
// that never came back.
//
// The settle window is not slack: read the instant the churn stops, this
// measures reconnect latency rather than node loss, because a session dropped
// in the final second is still inside its agent's reconnect backoff. Without
// it the check fails on a node that reappears a second later, which is not the
// failure it exists to catch.
func OfflineNodes(client *Client) ([]string, error) {
	deadline := time.Now().Add(ReconnectSettle)
	var lost []string
	for {
		rows, err := ListNodes(client)
		if err != nil {
			return nil, err
		}
		lost = lost[:0]
		for _, row := range rows {
			if row.State == NodeStateOffline {
				lost = append(lost, row.Name)
			}
		}
		if len(lost) == 0 || time.Now().After(deadline) {
			sort.Strings(lost)
			return lost, nil
		}
		time.Sleep(time.Second)
	}
}
