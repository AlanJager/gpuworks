//go:build multiproc

package l2

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// The lane tests in internal/integrationtest run the agent in the same process as
// the server, over real sockets. They can shape the link and restart the server
// instance, but they cannot kill an agent, so the process-boundary half of the
// stream contract — what the server does when a streaming peer disappears, and
// whether a restarted or reconnected peer gets its streams back — is only
// reachable here.

// metricPoint is one sample as the dashboards read it: a [t_ms, value] pair.
type metricPoint = apiwire.MetricPoint

// streamHarness is a server and an agent running as separate processes.
type streamHarness struct {
	base       string
	node       processNode
	nodeToken  string
	adminToken string
	stateDir   string
	serverLog  string
	agentLog   string
	agentBin   string
	serverBin  string
	server     *exec.Cmd
	agent      *exec.Cmd
	dataDir    string
	port       string
	configPath string
}

func startStreamServer(t *testing.T, h *streamHarness) *exec.Cmd {
	t.Helper()
	args := []string{"serve", "--addr", "127.0.0.1:" + h.port, "--data", h.dataDir}
	if h.configPath != "" {
		args = append(args, "--config", h.configPath)
	}
	server := serverCommand(t, "multiproc-password", h.serverBin, args...)
	logFile, err := os.Create(h.serverLog)
	if err != nil {
		t.Fatal(err)
	}
	server.Stdout, server.Stderr = logFile, logFile
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	waitHTTP(t, h.base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool {
		return resp.StatusCode == http.StatusUnauthorized
	}, h.serverLog)
	return server
}

func startStreamAgent(t *testing.T, h *streamHarness) *exec.Cmd {
	t.Helper()
	agent := exec.Command(h.agentBin, "run", "--server", h.base, "--token", h.nodeToken, "--state", h.stateDir)
	agent.Env = append(os.Environ(), "GPUWORKS_FAKE_GPU=A100x2,24576", "GPUWORKS_RUNTIME_DRIVER=none", "GPUWORKS_LOG_LEVEL=warn")
	logFile, err := os.OpenFile(h.agentLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	agent.Stdout, agent.Stderr = logFile, logFile
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	return agent
}

func newStreamHarness(t *testing.T) *streamHarness {
	t.Helper()
	return newStreamHarnessWithConfig(t, "")
}

func newStreamHarnessWithConfig(t *testing.T, configPath string) *streamHarness {
	t.Helper()
	dir := t.TempDir()
	h := &streamHarness{
		configPath: configPath,
		port:       freePort(t),
		dataDir:    filepath.Join(dir, "data"),
		stateDir:   filepath.Join(dir, "agent-state"),
		serverLog:  filepath.Join(dir, "server.log"),
		agentLog:   filepath.Join(dir, "agent.log"),
		serverBin:  buildBinary(t, "./cmd/gpuworks"),
		agentBin:   buildBinary(t, "./cmd/gpuworks-agent"),
	}
	h.base = "http://127.0.0.1:" + h.port
	h.server = startStreamServer(t, h)
	t.Cleanup(func() {
		stopProcess(h.agent)
		stopProcess(h.server)
	})
	h.adminToken = login(t, h.base)
	h.node, h.nodeToken = createNode(t, h.base, h.adminToken, "stream-node")
	h.agent = startStreamAgent(t, h)
	waitNode(t, h.base, h.adminToken, func(node processNode) bool {
		return node.UUID == h.node.UUID && node.State == "online"
	})
	return h
}

// killProcess ends a process abruptly: the peer disappears without closing
// anything, which is the condition these tests are about.
func killProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill process: %v", err)
	}
	_, _ = cmd.Process.Wait()
}

// metricSamples reads the samples the server holds for a node metric.
func metricSamples(t *testing.T, h *streamHarness, metric string) []metricPoint {
	t.Helper()
	q := url.Values{}
	q.Set("metric", metric)
	q.Set("range", "1h")
	q.Set("step", "1s")
	q.Set("node", h.node.UUID)
	req, err := http.NewRequest(http.MethodGet, h.base+"/api/v1/metrics/query?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.adminToken)
	resp, err := processHTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("metrics query status=%d body=%s", resp.StatusCode, body)
	}
	var series []apiwire.MetricSeries
	if err := apiwire.ReadList(resp.Body, &series); err != nil {
		t.Fatal(err)
	}
	var out []metricPoint
	for _, item := range series {
		out = append(out, item.Points...)
	}
	return out
}

// waitMetricAfter waits until the server holds a sample newer than afterMS. A
// sample the agent sent proves its metrics stream carried data across the
// process boundary at that moment.
func waitMetricAfter(t *testing.T, h *streamHarness, metric string, afterMS int64, what string) metricPoint {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	var last metricPoint
	var seen int
	for time.Now().Before(deadline) {
		samples := metricSamples(t, h, metric)
		seen = len(samples)
		for _, sample := range samples {
			if sample.TimeMS > last.TimeMS {
				last = sample
			}
		}
		if last.TimeMS > afterMS {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
	logs, _ := os.ReadFile(h.agentLog)
	t.Fatalf("%s: no sample newer than %d after 40s (%d samples seen)\nserver log:\n%s\nagent log:\n%s",
		what, afterMS, seen, readFileForLog(t, h.serverLog), logs)
	return metricPoint{}
}

func readFileForLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "(unreadable: " + err.Error() + ")"
	}
	return string(data)
}

// kill -9 an agent while it holds the metrics stream open: the server must drop
// the dead session, and a replacement process must be able to stream again. The
// samples from the first process stay queryable, so the client never sees a hole
// in the history.
func TestAgentKilledMidStreamReconnectsAndKeepsStreaming(t *testing.T) {
	h := newStreamHarness(t)
	const metric = "gpuworks_node_mem_total_bytes"

	before := waitMetricAfter(t, h, metric, 0, "first process")
	killProcess(t, h.agent)

	// The stream died with the process. The server learns that from the
	// connection, not from a timeout, so the node leaves "online" without
	// waiting for a heartbeat.
	waitNode(t, h.base, h.adminToken, func(node processNode) bool {
		return node.UUID == h.node.UUID && node.State == "offline"
	})
	afterKill := metricSamples(t, h, metric)
	for _, sample := range afterKill {
		if sample.TimeMS > before.TimeMS {
			t.Fatalf("a killed agent kept streaming: sample at %d is newer than %d", sample.TimeMS, before.TimeMS)
		}
	}

	// The same node identity comes back as a new process and streams again.
	h.agent = startStreamAgent(t, h)
	waitNode(t, h.base, h.adminToken, func(node processNode) bool {
		return node.UUID == h.node.UUID && node.State == "online"
	})
	after := waitMetricAfter(t, h, metric, before.TimeMS, "reconnected process")
	// Reconnection must preserve the same host fact. Darwin's portable
	// sysinfo collector reports unknown memory as zero; a positive-value
	// assertion tested Linux /proc support instead of stream recovery.
	if after.Value != before.Value {
		t.Fatalf("host memory changed across reconnect: before=%+v after=%+v", before, after)
	}

	// Both sides of the discontinuity are in the history.
	all := metricSamples(t, h, metric)
	var earliest, latest metricPoint
	for index, sample := range all {
		if index == 0 || sample.TimeMS < earliest.TimeMS {
			earliest = sample
		}
		if sample.TimeMS > latest.TimeMS {
			latest = sample
		}
	}
	if earliest.TimeMS > before.TimeMS || latest.TimeMS < after.TimeMS {
		t.Fatalf("history = %d..%d, want it to span %d..%d", earliest.TimeMS, latest.TimeMS, before.TimeMS, after.TimeMS)
	}
}

// Restarting the server must not end the node: the agent's reconnect loop
// re-establishes the connection and its streams, and the samples collected
// before the restart survive in the data directory.
func TestServerRestartKeepsAgentStreamsUsable(t *testing.T) {
	h := newStreamHarness(t)
	const metric = "gpuworks_node_mem_total_bytes"

	before := waitMetricAfter(t, h, metric, 0, "before server restart")
	stopProcess(h.server)

	// The agent is still running; only the server it dials is gone. A fresh
	// process on the same port and data directory must accept it back.
	h.server = startStreamServer(t, h)
	waitNode(t, h.base, h.adminToken, func(node processNode) bool {
		return node.UUID == h.node.UUID && node.State == "online"
	})

	after := waitMetricAfter(t, h, metric, before.TimeMS, "after server restart")
	history := metricSamples(t, h, metric)
	var kept bool
	for _, sample := range history {
		if sample.TimeMS == before.TimeMS {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("samples from before the restart are gone: history has %d point(s), latest %d", len(history), after.TimeMS)
	}
}
