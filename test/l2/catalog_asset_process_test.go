//go:build multiproc

package l2

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// TestCatalogAssetSurvivesRealServerAndAgentRestart proves the E2 persistence
// boundary with independently built processes. The server stores only the
// package reference in SQLite, the first push hydrates bytes, and a restarted
// agent reuses its SHA-addressed cache even if the server package copy is no
// longer available.
func TestCatalogAssetSurvivesRealServerAndAgentRestart(t *testing.T) {
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	dataDir := t.TempDir()
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	startServer := func() *exec.Cmd {
		command := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
		command.Stdout, command.Stderr = serverLog, serverLog
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		return command
	}
	server := startServer()
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(response *http.Response) bool { return response.StatusCode == http.StatusUnauthorized }, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "catalog-asset-process")

	recordPath := filepath.Join(t.TempDir(), "workload.json")
	agentState := filepath.Join(t.TempDir(), "agent-state")
	startAgent := func() *exec.Cmd {
		command := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", agentState)
		command.Env = append(os.Environ(),
			"GPUWORKS_FAKE_GPU=A100x1,81920",
			"GPUWORKS_FAKE_RUNTIME_RECORD="+recordPath,
			"GPUWORKS_LOG_LEVEL=warn",
		)
		command.Stdout, command.Stderr = io.Discard, io.Discard
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		return command
	}
	agent := startAgent()
	t.Cleanup(func() { stopProcess(agent) })
	node := waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State == "online" && candidate.GPUCount == 1
	})

	const modelID = "gpuworks/l2-catalog-asset"
	const revision = "0123456789abcdef0123456789abcdef01234567"
	modelBody, _ := json.Marshal(map[string]any{
		"source": "huggingface", "source_id": modelID, "revision": revision, "name": modelID,
		"browser_metadata": true, "weight_bytes": 7,
		"config_raw": `{"model_type":"fake","architectures":["FakeForCausalLM"],"max_position_embeddings":2048}`,
	})
	modelResponse, err := authedPost(t, base, adminToken, "/api/v1/models", modelBody)
	if err != nil {
		t.Fatal(err)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if modelResponse.StatusCode != http.StatusCreated || json.NewDecoder(modelResponse.Body).Decode(&model) != nil || model.ID == 0 {
		raw, _ := io.ReadAll(modelResponse.Body)
		modelResponse.Body.Close()
		t.Fatalf("create model status=%d body=%s model=%+v", modelResponse.StatusCode, raw, model)
	}
	modelResponse.Body.Close()
	modelPath := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelPath, "config.json"), []byte(`{"model_type":"fake","architectures":["FakeForCausalLM"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelPath, "model.safetensors"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	registerBody, _ := json.Marshal(map[string]any{"node_uuid": node.UUID, "path": modelPath})
	registerResponse, err := authedPost(t, base, adminToken, fmt.Sprintf("/api/v1/models/%d/register-path", model.ID), registerBody)
	if err != nil || registerResponse.StatusCode != http.StatusOK {
		var raw []byte
		if registerResponse != nil {
			raw, _ = io.ReadAll(registerResponse.Body)
			registerResponse.Body.Close()
		}
		t.Fatalf("register path status=%v body=%s err=%v", responseStatus(registerResponse), raw, err)
	}
	registerResponse.Body.Close()

	asset := []byte("print('real process asset')\n")
	assetSHA := fmt.Sprintf("%x", sha256.Sum256(asset))
	catalogModel := inferencecatalog.InferenceModel{ModelID: modelID, Source: "user", Architecture: "FakeForCausalLM", InputModalities: []string{"text"}, Specs: []inferencecatalog.InferenceSpec{{
		Name: "asset", Engine: "vllm", Image: "example/vllm:test",
		Weights:     inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: modelID, Revision: revision}}},
		WeightBytes: 7, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia"},
		Assets: []inferencecatalog.Asset{{File: "assets/serve.py", SHA256: assetSHA, UnpackTo: "/opt/gpuworks/serve.py"}},
	}}}
	packageFS, err := inferencecatalog.BuildPackageFS("2026-09-01T00:00:00Z", inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{catalogModel}}, map[string][]byte{"models/" + modelID + "/assets/serve.py": asset})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, packageFS); err != nil {
		t.Fatal(err)
	}
	importRequest, _ := http.NewRequest(http.MethodPost, base+"/api/v1/inference-templates/import", bytes.NewReader(archive.Bytes()))
	importRequest.Header.Set("Authorization", "Bearer "+adminToken)
	importRequest.Header.Set("Content-Type", "application/gzip")
	importResponse, err := processHTTPClient.Do(importRequest)
	if err != nil {
		t.Fatal(err)
	}
	var imported struct {
		PackageDigest string `json:"package_digest"`
	}
	if importResponse.StatusCode != http.StatusOK || json.NewDecoder(importResponse.Body).Decode(&imported) != nil || imported.PackageDigest == "" {
		raw, _ := io.ReadAll(importResponse.Body)
		importResponse.Body.Close()
		t.Fatalf("import status=%d body=%s parsed=%+v", importResponse.StatusCode, raw, imported)
	}
	importResponse.Body.Close()
	templateID := findTemplateID(t, base, adminToken, modelID)
	deployBody, _ := json.Marshal(map[string]any{
		"template_id": templateID, "template_spec_name": "asset", "model_id": model.ID,
		"node_uuid": node.UUID, "served_name": "l2-catalog-asset",
	})
	deploymentResponse, err := authedPost(t, base, adminToken, "/api/v1/deployments", deployBody)
	if err != nil || deploymentResponse.StatusCode != http.StatusCreated {
		var raw []byte
		if deploymentResponse != nil {
			raw, _ = io.ReadAll(deploymentResponse.Body)
			deploymentResponse.Body.Close()
		}
		t.Fatalf("deploy status=%v body=%s err=%v", responseStatus(deploymentResponse), raw, err)
	}
	deploymentResponse.Body.Close()
	first := waitRecordedCatalogAssetWorkload(t, recordPath, asset)

	stopProcess(agent)
	agent = nil
	stopProcess(server)
	server = nil
	if err := os.Remove(recordPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	packagePath := filepath.Join(dataDir, "catalog-packages", imported.PackageDigest)
	if err := os.Rename(packagePath, packagePath+".intentionally-unavailable"); err != nil {
		t.Fatal(err)
	}
	server = startServer()
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(response *http.Response) bool { return response.StatusCode == http.StatusUnauthorized }, serverLogPath)
	agent = startAgent()
	waitNode(t, base, adminToken, func(candidate processNode) bool { return candidate.UUID == node.UUID && candidate.State == "online" })
	second := waitRecordedCatalogAssetWorkload(t, recordPath, asset)
	if !reflectWorkloadLaunch(first, second) {
		t.Fatalf("restart changed workload launch:\nfirst=%+v\nsecond=%+v", first, second)
	}
}

func findTemplateID(t *testing.T, base, token, modelID string) uint {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, base+"/api/v1/inference-templates/model/"+modelID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := processHTTPClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var detail struct {
		ID uint `json:"id"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&detail) != nil || detail.ID == 0 {
		t.Fatalf("template detail status=%d detail=%+v", response.StatusCode, detail)
	}
	return detail.ID
}

func waitRecordedCatalogAssetWorkload(t *testing.T, recordPath string, wantAsset []byte) agentruntime.WorkloadSpec {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(recordPath)
		if err == nil {
			var workload agentruntime.WorkloadSpec
			if json.Unmarshal(data, &workload) == nil && !strings.HasPrefix(workload.Name, "gpuworks-partition-probe-") {
				for _, mount := range workload.Mounts {
					if mount.Target != "/opt/gpuworks/serve.py" || !mount.ReadOnly {
						continue
					}
					actual, readErr := os.ReadFile(mount.Source)
					if readErr == nil && bytes.Equal(actual, wantAsset) {
						return workload
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("recorded workload never mounted the exact catalog asset: %s", recordPath)
	return agentruntime.WorkloadSpec{}
}

func reflectWorkloadLaunch(left, right agentruntime.WorkloadSpec) bool {
	if left.Image != right.Image || !equalStrings(left.Entrypoint, right.Entrypoint) || !equalStrings(left.Cmd, right.Cmd) {
		return false
	}
	if len(left.Mounts) != len(right.Mounts) {
		return false
	}
	for index := range left.Mounts {
		if left.Mounts[index].Target != right.Mounts[index].Target || left.Mounts[index].ReadOnly != right.Mounts[index].ReadOnly {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
