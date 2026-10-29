//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/agentbin"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

const upgradeTargetVersion = "v0.6.0-l2-target"

// Only upgrade tests need release identities. Normal process fixtures retain
// their development version; binaries under test are private writable copies.
func upgradeProcessBinaries(t *testing.T) (server, oldAgent, dataDir string) {
	t.Helper()
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	server = buildUpgradeBinary(t, "./cmd/gpuworks", upgradeTargetVersion)
	target := buildUpgradeBinary(t, "./cmd/gpuworks-agent", upgradeTargetVersion)
	old := buildUpgradeBinary(t, "./cmd/gpuworks-agent", "v0.5.0-l2-old")
	dataDir = t.TempDir()
	dl := filepath.Join(dataDir, "dl")
	if err := os.MkdirAll(dl, 0755); err != nil {
		t.Fatal(err)
	}
	name := "gpuworks-agent-" + runtime.GOOS + "-" + runtime.GOARCH
	targetPath := filepath.Join(dl, name)
	copyUpgradeBinary(t, target, targetPath)
	if err := agentbin.WriteManifest(upgradeTargetVersion, filepath.Join(dl, agentbin.ManifestName), []string{targetPath}); err != nil {
		t.Fatal(err)
	}
	oldAgent = filepath.Join(t.TempDir(), "gpuworks-agent")
	copyUpgradeBinary(t, old, oldAgent)
	return
}

func copyUpgradeBinary(t *testing.T, source, dest string) {
	t.Helper()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, raw, 0755); err != nil {
		t.Fatal(err)
	}
}

func buildUpgradeBinary(t *testing.T, pkg, version string) string {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	key := pkg + "@" + version
	if got, ok := builtBins[key]; ok {
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.path
	}
	out := filepath.Join(sharedBuildRootLocked(t), filepath.Base(pkg)+"-"+version)
	args := []string{"build", "-trimpath", "-ldflags", "-X gpuworks.ai/gpuworks/internal/version.Version=" + version}
	if os.Getenv("GOCOVERDIR") != "" {
		args = append(args, "-cover", "-coverpkg=./cmd/...,./internal/...,./pkg/...")
	}
	args = append(args, "-o", out, pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("build upgrade binary: %w\n%s", err, output)
	}
	builtBins[key] = buildResult{path: out, err: err}
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func upgradeJSON(t *testing.T, base, token, method, path string, body any, want int, result any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+"/api/v1"+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := processHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status=%d body=%s", method, path, resp.StatusCode, raw)
	}
	if result != nil {
		if err := json.Unmarshal(raw, result); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
}

func TestAgentBatchUpgradeConfirmsBothReplacementProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("requires Linux self-upgrade")
		}
		t.Skip("requires Linux self-upgrade")
	}
	serverBin, oldAgent, dataDir := upgradeProcessBinaries(t)
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, "upgrade-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
	serverLog, err := os.Create(filepath.Join(t.TempDir(), "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer serverLog.Close()
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopProcess(server)
	defer func() {
		if t.Failed() {
			raw, _ := os.ReadFile(serverLog.Name())
			t.Logf("server log:\n%s", raw)
		}
	}()
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized })
	token := loginWithPassword(t, base, "upgrade-password")
	var binaries []string
	for i := 0; i < 2; i++ {
		node, nodeToken := createNode(t, base, token, fmt.Sprintf("batch-upgrade-%d", i))
		binary := filepath.Join(t.TempDir(), "gpuworks-agent")
		copyUpgradeBinary(t, oldAgent, binary)
		binaries = append(binaries, binary)
		agent := exec.Command(binary, "run", "--server", base, "--token", nodeToken, "--state", t.TempDir())
		agent.Env = append(os.Environ(), "GPUWORKS_FAKE_GPU=A100x1,24576", "GPUWORKS_RUNTIME_DRIVER=none", "GPUWORKS_LOG_LEVEL=warn")
		agent.Stdout, agent.Stderr = serverLog, serverLog
		if err := agent.Start(); err != nil {
			t.Fatal(err)
		}
		defer stopProcess(agent)
		waitNode(t, base, token, func(n processNode) bool { return n.UUID == node.UUID && n.State == "online" })
	}
	var preview dto.AgentUpgradePreview
	upgradeJSON(t, base, token, http.MethodPost, "/agent-upgrades/preview", map[string]string{"scope": "all"}, http.StatusOK, &preview)
	if preview.Counts["eligible"] != 2 || len(preview.NodeUUIDs) != 2 || preview.TargetVersion != upgradeTargetVersion {
		t.Fatalf("preview=%+v", preview)
	}
	var run dto.AgentUpgradeRun
	upgradeJSON(t, base, token, http.MethodPost, "/agent-upgrades", dto.AgentUpgradeCreateRequest{NodeUUIDs: preview.NodeUUIDs, ExpectedTargetVersion: preview.TargetVersion, ExpectedManifestFingerprint: preview.ManifestFingerprint, IdempotencyKey: "two-real-processes"}, http.StatusAccepted, &run)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		upgradeJSON(t, base, token, http.MethodGet, fmt.Sprintf("/agent-upgrades/%d", run.ID), nil, http.StatusOK, &run)
		if run.Counts.Succeeded == 2 {
			break
		}
		if run.Counts.Failed+run.Counts.Unconfirmed+run.Counts.Interrupted > 0 {
			t.Fatalf("batch failed: %+v", run)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if run.Counts.Succeeded != 2 || run.Counts.Processed != 2 || run.Counts.Active != 0 {
		t.Fatalf("batch did not finish: %+v", run)
	}
	var items dto.AgentUpgradeItemPage
	upgradeJSON(t, base, token, http.MethodGet, fmt.Sprintf("/agent-upgrades/%d/items", run.ID), nil, http.StatusOK, &items)
	if len(items.Items) != 2 {
		t.Fatalf("items=%+v", items)
	}
	for _, item := range items.Items {
		if item.Phase != "succeeded" || item.RegisteredAt == nil || item.ObservedVersion != upgradeTargetVersion || item.CurrentVersion != upgradeTargetVersion {
			t.Fatalf("no formal target registration: %+v", item)
		}
	}
	for _, binary := range binaries {
		waitUpgradeConfirmed(t, binary)
	}
}

// Local installer confirmation alone does not prove that the durable control
// plane observed a fresh, normal target-version session.
func waitUpgradeRunFromResponse(t *testing.T, base, token string, response []byte) {
	t.Helper()
	var accepted struct {
		RunID uint `json:"run_id"`
	}
	if err := json.Unmarshal(response, &accepted); err != nil || accepted.RunID == 0 {
		t.Fatalf("missing durable upgrade run: %s (%v)", response, err)
	}
	var run dto.AgentUpgradeRun
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		upgradeJSON(t, base, token, http.MethodGet, fmt.Sprintf("/agent-upgrades/%d", accepted.RunID), nil, http.StatusOK, &run)
		if run.Counts.Succeeded == 1 {
			return
		}
		if run.Counts.Failed+run.Counts.Unconfirmed+run.Counts.Interrupted > 0 {
			t.Fatalf("single upgrade failed: %+v", run)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("single upgrade lacks formal target confirmation: %+v", run)
}
