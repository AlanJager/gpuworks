//go:build multiproc

package l2

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestOrphanCleanupGatedByServerSetting reproduces the gpu3 incident shape
// end-to-end against real server and agent processes: a gpuworks-named
// container that no desired state claims (an "orphan" from this control
// plane's point of view) sits on the shared docker daemon.
//
// The sweep must leave it alone while the orphan_cleanup setting is unset —
// including across an agent reconnect, which is exactly how a second gpuworks
// agent used to destroy a live deployment's containers — and must remove it
// once an operator flips the setting, with the resync reaching the agent
// without a reconnect.
func TestOrphanCleanupGatedByServerSetting(t *testing.T) {
	realDocker, err := exec.LookPath("docker")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker required in strict mode: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker daemon unavailable in strict mode: %v", err)
		}
		t.Skipf("docker daemon unavailable: %v", err)
	}
	root := repoRoot()
	if err := ensureFakeEngineImage(t, root); err != nil {
		t.Fatal(err)
	}
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	dataDir := t.TempDir()
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
	server.Stdout, server.Stderr = os.Stdout, os.Stderr
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(resp *http.Response) bool { return resp.StatusCode == http.StatusUnauthorized })
	adminToken := login(t, base)
	_, nodeToken := createNode(t, base, adminToken, "orphan-cleanup-node")

	// The orphan: a long-running gpuworks-named container started outside any
	// gpuworks control plane, exactly like a leftover from a lost database or a
	// foreign agent.
	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"orphan-probe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	owner := fmt.Sprint(time.Now().UnixNano())
	orphan := "gpuworks-orphanl2-" + owner
	dockerScope := orphanDockerScope(t, realDocker, orphan, owner)
	cleanup := func(name, label string) {
		t.Cleanup(func() {
			listed, err := exec.Command(realDocker, "container", "ls", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}").CombinedOutput()
			if err != nil {
				t.Errorf("list owned fixture %s during cleanup: %v: %s", name, err, listed)
				return
			}
			if strings.TrimSpace(string(listed)) == "" {
				return
			}
			actual, err := exec.Command(realDocker, "inspect", "--format", `{{index .Config.Labels "gpuworks.l2-orphan"}}`, name).CombinedOutput()
			if err != nil {
				if !strings.Contains(string(actual), "No such object") && !strings.Contains(string(actual), "No such container") {
					t.Errorf("inspect owned fixture %s during cleanup: %v: %s", name, err, actual)
				}
				return
			}
			if strings.TrimSpace(string(actual)) != label {
				t.Errorf("owned fixture %s label changed; refusing cleanup", name)
				return
			}
			if out, err := exec.Command(realDocker, "rm", "-f", name).CombinedOutput(); err != nil {
				t.Errorf("cleanup owned fixture %s: %v: %s", name, err, out)
			}
		})
	}
	cleanup(orphan, owner)
	if out, err := exec.Command(realDocker, "run", "-d", "--network", "none", "--label", "gpuworks.l2-orphan="+owner, "--name", orphan,
		"-v", modelDir+":/m:ro", "gpuworks/fake-engine:test", "--model", "/m", "--port", "8123").CombinedOutput(); err != nil {
		t.Fatalf("start orphan container: %v: %s", err, out)
	}
	containerRunning := func() bool {
		out, err := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", orphan).CombinedOutput()
		return err == nil && strings.TrimSpace(string(out)) == "true"
	}
	if !containerRunning() {
		t.Fatal("orphan probe container did not start")
	}

	// A second container created by this test represents another logical node.
	// It must remain invisible to this agent even after cleanup is enabled.
	sentinel := "gpuworks-orphanl2-sentinel-" + owner
	cleanup(sentinel, owner+"-sentinel")
	if out, err := exec.Command(realDocker, "run", "-d", "--network", "none", "--label", "gpuworks.l2-orphan="+owner+"-sentinel", "--name", sentinel,
		"-v", modelDir+":/m:ro", "gpuworks/fake-engine:test", "--model", "/m", "--port", "8123").CombinedOutput(); err != nil {
		t.Fatalf("start foreign sentinel: %v: %s", err, out)
	}

	state := filepath.Join(t.TempDir(), "agent-state")
	var agentsMu sync.Mutex
	var agents []*exec.Cmd
	startAgent := func() *exec.Cmd {
		agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", state)
		agent.Env = append(os.Environ(), "GPUWORKS_FAKE_GPU=A100x1,24576", "GPUWORKS_FAKE_GPU_NO_INJECT=1", "GPUWORKS_RUNTIME_DRIVER=docker", "GPUWORKS_LOG_LEVEL=warn", "PATH="+dockerScope+string(os.PathListSeparator)+os.Getenv("PATH"))
		agent.Stdout, agent.Stderr = os.Stdout, os.Stderr
		if err := agent.Start(); err != nil {
			t.Fatal(err)
		}
		agentsMu.Lock()
		agents = append(agents, agent)
		agentsMu.Unlock()
		return agent
	}
	t.Cleanup(func() {
		agentsMu.Lock()
		started := append([]*exec.Cmd(nil), agents...)
		agentsMu.Unlock()
		for _, process := range started {
			stopProcess(process)
		}
	})

	// First state sync with the setting unset must not touch the orphan.
	agent := startAgent()
	waitNode(t, base, adminToken, func(n processNode) bool { return n.State == "online" && n.GPUCount == 1 })
	time.Sleep(3 * time.Second)
	if !containerRunning() {
		t.Fatal("orphan container was swept while orphan_cleanup was unset")
	}

	// A reconnect replays the full desired state; the sweep decision must
	// still respect the setting (this is the exact sequence that destroyed
	// gpu3's deployment 4 when a second agent synced on the same daemon).
	stopProcess(agent)
	startAgent()
	waitNode(t, base, adminToken, func(n processNode) bool { return n.State == "online" && n.GPUCount == 1 })
	time.Sleep(3 * time.Second)
	if !containerRunning() {
		t.Fatal("orphan container was swept on reconnect while orphan_cleanup was unset")
	}

	// Flipping the setting resyncs online nodes immediately; the sweep then
	// reclaims the orphan without another reconnect.
	req, _ := http.NewRequest(http.MethodPut, base+"/api/v1/settings/orphan_cleanup", strings.NewReader("true"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := processHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable orphan_cleanup: status %d", resp.StatusCode)
	}
	deadline := time.Now().Add(20 * time.Second)
	for containerRunning() {
		if time.Now().After(deadline) {
			t.Fatal("orphan container survived 20s after orphan_cleanup was enabled")
		}
		time.Sleep(500 * time.Millisecond)
	}
	// The daemon removes the container after stopping it; under load the
	// removal can trail the stop by a beat, so poll for it instead of
	// inspecting once.
	removed := time.Now().Add(10 * time.Second)
	for {
		out, err := exec.Command(realDocker, "container", "ls", "-a", "--filter", "name=^/"+orphan+"$", "--format", "{{.ID}}").CombinedOutput()
		if err != nil {
			t.Fatalf("list exact orphan after sweep: %v: %s", err, out)
		}
		if strings.TrimSpace(string(out)) == "" {
			break
		}
		if time.Now().After(removed) {
			t.Fatalf("orphan container still exists 10s after the sweep: %s", strings.TrimSpace(string(out)))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if out, err := exec.Command(realDocker, "inspect", "--format", "{{.State.Running}}", sentinel).Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("foreign sentinel was affected by orphan cleanup: %v: %s", err, out)
	}
	fmt.Fprintln(os.Stderr, "orphan cleanup L2: sweep reclaimed", orphan, "and retained foreign sentinel", sentinel)
}
