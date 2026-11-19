package slurm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// An enrolled agent tells the server which nodes its Slurm cluster has
// (sinfo -a), so the cluster entry can show how many GPU nodes lack an agent,
// and takes the entry's current settings from the answer. Every agent of an
// entry reports, which costs the controller one sinfo per agent per interval;
// the interval is long and spread by jitter for that, and the server answers
// digests it already holds without reading anything (POST
// /api/v1/slurm/gpu-nodes). The membership and the states have a digest each:
// states change all the time and must not make the server store the list.

// ReportInterval is how often an agent reports, and so the longest an edit of
// the entry's settings takes to reach it (the server's report_interval_seconds).
const ReportInterval = 10 * time.Minute

// reportRetry is the wait after a report that failed.
const reportRetry = 2 * time.Minute

// ClusterNode is one node of the cluster as sinfo lists it. A node whose GRES
// has gpu is a GPU node, with its state, reason and GPU count; every other
// node (no GRES, or one without gpu) is listed by name alone: the server must
// know Slurm still has it, but expects no agent there.
type ClusterNode struct {
	Name       string `json:"name"`
	ListedOnly bool   `json:"listed_only,omitempty"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	GPUs       int    `json:"gpus"`
}

// sinfoFormat prints a node's name, long state, GRES and reason; the reason,
// which holds spaces and may hold the separator, goes last.
const sinfoFormat = "%N|%T|%G|%E"

var gresGPUCount = regexp.MustCompile(`(?:^|,)gpu(?::[^:,(]+)?:(\d+)`)

// ClusterNodes lists every node of the cluster, one entry each (sinfo prints a
// node once per partition), by name. -a includes the partitions hidden from
// the user the agent runs as: a node there is still in the cluster.
func ClusterNodes(ctx context.Context, run Runner) ([]ClusterNode, error) {
	out, err := run(ctx, "sinfo", "-a", "-h", "-N", "-o", sinfoFormat)
	if err != nil {
		return nil, err
	}
	return parseClusterNodes(out), nil
}

func parseClusterNodes(out []byte) []ClusterNode {
	seen := map[string]bool{}
	nodes := []ClusterNode{}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "|", 4)
		if len(parts) < 3 || parts[0] == "" || seen[parts[0]] {
			continue
		}
		seen[parts[0]] = true
		m := gresGPUCount.FindStringSubmatch(parts[2])
		if m == nil && strings.Contains(parts[2], "gpu") {
			m = []string{"", "0"} // a gpu GRES whose count sinfo does not print
		}
		if m == nil {
			nodes = append(nodes, ClusterNode{Name: parts[0], ListedOnly: true})
			continue
		}
		count, _ := strconv.Atoi(m[1])
		n := ClusterNode{Name: parts[0], State: strings.ToLower(strings.TrimRight(parts[1], "*~#!%$@^-")), GPUs: count}
		if len(parts) == 4 && parts[3] != "none" && parts[3] != "(null)" {
			n.Reason = strings.TrimSpace(parts[3])
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes
}

// Digest identifies a list's membership: which nodes the cluster has, which
// of them have GPUs and how many. The server holds the one it last stored and
// skips a report that repeats it. State and reason are not in it, they change
// all the time (StateDigest).
func Digest(nodes []ClusterNode) string {
	h := sha256.New()
	for _, n := range nodes {
		fmt.Fprintf(h, "%s\x00%t\x00%d\n", n.Name, n.ListedOnly, n.GPUs)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// StateDigest identifies the state and reason of the GPU nodes.
func StateDigest(nodes []ClusterNode) string {
	h := sha256.New()
	for _, n := range nodes {
		if !n.ListedOnly {
			fmt.Fprintf(h, "%s\x00%s\x00%s\n", n.Name, n.State, n.Reason)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Reporter reports the cluster's GPU nodes to the server.
type Reporter struct {
	// Server is the server's address, Token gives this node's own token as it
	// is now: the agent may be issued a new one at registration, or enroll
	// again after the server forgot the old one, and every report carries the
	// current token.
	Server  string
	Token   func() string
	HTTP    *http.Client
	Command Runner
	Log     *slog.Logger
	// Apply is given the entry's settings from every answer.
	Apply func(Settings)
	// Interval is the time between reports (ReportInterval); Jitter spreads them.
	Interval time.Duration
	// Wait is how long to wait before a report; nil waits on a timer. A test
	// substitutes its own.
	Wait func(context.Context, time.Duration) bool
}

type reportAnswer struct {
	NeedNodes bool     `json:"need_nodes"`
	Settings  Settings `json:"settings"`
}

// Once lists the nodes and reports them: the digests first, the nodes when the
// server wants that list. The answer's settings are applied.
func (r *Reporter) Once(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	nodes, err := ClusterNodes(ctx, r.Command)
	if err != nil {
		return fmt.Errorf("list the cluster's nodes: %w", err)
	}
	digests := map[string]any{"digest": Digest(nodes), "state_digest": StateDigest(nodes)}
	answer, err := r.post(ctx, digests)
	if err != nil {
		return err
	}
	if answer.NeedNodes {
		digests["nodes"] = nodes
		if answer, err = r.post(ctx, digests); err != nil {
			return err
		}
	}
	if r.Apply != nil {
		r.Apply(answer.Settings)
	}
	return nil
}

func (r *Reporter) post(ctx context.Context, body map[string]any) (reportAnswer, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.Server, "/")+"/api/v1/slurm/gpu-nodes", bytes.NewReader(raw))
	if err != nil {
		return reportAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	token := ""
	if r.Token != nil {
		token = r.Token()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	httpc := r.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return reportAnswer{}, fmt.Errorf("report to %s: %w", r.Server, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return reportAnswer{}, fmt.Errorf("report to %s: HTTP %d: %s", r.Server, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var answer reportAnswer
	if err := json.Unmarshal(data, &answer); err != nil {
		return reportAnswer{}, fmt.Errorf("report to %s: unexpected answer %q", r.Server, data)
	}
	return answer, nil
}

// Run reports until ctx ends: first a little after the start (the node is
// being set up, and the answer carries the settings), then every Interval,
// each wait stretched or shortened by up to a fifth so the agents of a cluster
// do not all ask the controller at once. A failure is tried again sooner.
func (r *Reporter) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = ReportInterval
	}
	wait := r.Wait
	if wait == nil {
		wait = sleep
	}
	next := time.Duration(20+rand.IntN(40)) * time.Second
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	for wait(ctx, next) {
		if err := r.Once(ctx); err != nil {
			log.Warn("could not report the Slurm cluster's GPU nodes; trying again", "err", err, "in", reportRetry.String())
			next = reportRetry
			continue
		}
		next = interval - interval/5 + time.Duration(rand.Int64N(int64(interval)*2/5+1))
	}
}

// sleep waits d, and says whether ctx is still alive.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
