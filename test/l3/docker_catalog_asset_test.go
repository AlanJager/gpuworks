//go:build docker

package l3

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestDockerCatalogPackageAssetLaunch proves the complete executable-catalog
// path with the real Docker driver. The first deployment imports and extracts
// a model-owned archive; the second deployment receives the same immutable
// asset after the hub has acknowledged its digest and must reuse the agent
// cache without publishing a new copy.
func TestDockerCatalogPackageAssetLaunch(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if err := ensureFakeImage(t, root); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	if err := ensureFakePreStartImage(root); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	registerFakePreStartEngine(t)

	env := testenv.New(t)
	node := env.AddNode("docker-catalog-asset",
		testenv.WithGPUs(1, testenv.GPUSpec{Model: "NVIDIA A100 80GB", VRAMMB: 81920, ComputeCapability: "8.0"}),
		testenv.WithDriver(dockerdrv.New()),
	)
	env.WaitNodeOnline(t, node.Name)
	// The template's spec is matched against the node's GPUs at create time;
	// the inventory lands in the first status.node report, after online.
	env.WaitNodeGPUs(t, node.Name, 1)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	const modelID = "gpuworks/l3-catalog-asset"
	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: modelID, Name: modelID, Arch: "qwen3", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: modelDir}); err != nil {
		t.Fatal(err)
	}

	assetContents := catalogRuntimeArchive(t)
	assetDigest := fmt.Sprintf("%x", sha256.Sum256(assetContents))
	archive := executableCatalogPackage(t, modelID, assetContents, assetDigest)
	api := env.API(t)
	request, err := http.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/gzip")
	response, err := api.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("import executable catalog package: %d %s", response.StatusCode, body)
	}
	response.Body.Close()
	catalogModel, err := env.Store().InferenceModelByModelID(modelID)
	if err != nil || catalogModel.PackageDigest == "" {
		t.Fatalf("catalog model=%+v err=%v", catalogModel, err)
	}

	cacheRoot := filepath.Join(node.StateDir(), "catalog-assets")
	t.Cleanup(func() { restoreWritableTree(cacheRoot) })
	firstDeployment, firstContainer := deployCatalogAsset(t, env, api, catalogModel.ID, "catalog-asset-first")
	assertCatalogAssetContainer(t, firstContainer)
	cacheDirectory := filepath.Join(cacheRoot, assetDigest+".dir")
	cacheFile := filepath.Join(cacheDirectory, "prepare.sh")
	firstInfo, err := os.Stat(cacheFile)
	if err != nil {
		t.Fatalf("stat first published cache: %v", err)
	}
	if firstInfo.Mode().Perm() != 0o444 {
		t.Fatalf("cache file mode=%o, want 0444", firstInfo.Mode().Perm())
	}
	firstModTime := firstInfo.ModTime()

	deleteCatalogAssetDeployment(t, env, api, firstDeployment)
	secondDeployment, secondContainer := deployCatalogAsset(t, env, api, catalogModel.ID, "catalog-asset-cache-hit")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", secondContainer).Run() })
	assertCatalogAssetContainer(t, secondContainer)
	secondInfo, err := os.Stat(cacheFile)
	if err != nil {
		t.Fatalf("stat reused cache: %v", err)
	}
	if !secondInfo.ModTime().Equal(firstModTime) {
		t.Fatalf("content-addressed cache was republished: first=%s second=%s", firstModTime, secondInfo.ModTime())
	}
	marker, err := os.ReadFile(filepath.Join(cacheRoot, assetDigest+".complete"))
	if err != nil || strings.TrimSpace(string(marker)) != assetDigest {
		t.Fatalf("cache marker=%q err=%v", marker, err)
	}
	deleteCatalogAssetDeployment(t, env, api, secondDeployment)
}

func catalogRuntimeArchive(t *testing.T) []byte {
	t.Helper()
	files := map[string][]byte{
		"prepare.sh":  []byte("set -eu\ncat /opt/gpuworks/runtime/payload.txt > /tmp/catalog-asset-marker\nif touch /opt/gpuworks/runtime/must-remain-read-only 2>/tmp/catalog-asset-write-error; then exit 42; fi\n"),
		"payload.txt": []byte("catalog-package-ready\n"),
	}
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range []string{"payload.txt", "prepare.sh"} {
		contents := files[name]
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(contents))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func executableCatalogPackage(t *testing.T, modelID string, asset []byte, digest string) []byte {
	t.Helper()
	model := inferencecatalog.InferenceModel{
		ModelID: modelID, Source: "user", DisplayTitle: "L3 catalog asset", InputModalities: []string{"text"}, ContextLength: 2048,
		Specs: []inferencecatalog.InferenceSpec{{
			Name: "nvidia-general", DisplayProfile: "general", Scenario: "balanced", Engine: "fake-engine", Image: "gpuworks/fake-engine:prestart",
			Weights:     inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: modelID}}},
			WeightBytes: 1 << 30, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=7.5", DisplayTarget: "nvidia"},
			Assets:       []inferencecatalog.Asset{{File: "assets/runtime.tar.gz", SHA256: digest, UnpackTo: "/opt/gpuworks/runtime"}},
			PreStartArgv: []string{"sh", "/opt/gpuworks/runtime/prepare.sh"},
		}},
	}
	modelYAML, err := inferencecatalog.MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	packageFS := fstest.MapFS{
		"manifest.yaml": {Data: []byte("schema_version: 1\npublished_at: 2026-08-31T00:00:00Z\nmodels: 1\nspecs: 1\n")},
		"models/gpuworks/l3-catalog-asset/model.yaml":            {Data: modelYAML},
		"models/gpuworks/l3-catalog-asset/assets/runtime.tar.gz": {Data: asset},
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, packageFS); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func deployCatalogAsset(t *testing.T, env *testenv.Env, api *testenv.APIClient, templateID uint, servedName string) (uint, string) {
	t.Helper()
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	body, _ := json.Marshal(map[string]any{
		"template_id": templateID, "template_spec_name": "nvidia-general", "node_uuid": nodes[0].UUID, "served_name": servedName,
	})
	response, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create catalog deployment: %d %s", response.StatusCode, data)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	var containerID string
	env.Eventually(t, 45*time.Second, func() bool {
		instances, _ := env.Store().ListInstances()
		for _, instance := range instances {
			if instance.DeploymentID != created.ID {
				continue
			}
			if instance.State == proto.InstFailed {
				t.Fatalf("catalog asset deployment failed: %+v", instance)
			}
			if instance.State == proto.InstRunning && instance.ContainerID != "" {
				containerID = instance.ContainerID
				return true
			}
		}
		return false
	})
	return created.ID, containerID
}

func assertCatalogAssetContainer(t *testing.T, containerID string) {
	t.Helper()
	command := "test \"$(cat /tmp/catalog-asset-marker)\" = catalog-package-ready && test ! -e /opt/gpuworks/runtime/must-remain-read-only"
	if output, err := exec.Command("docker", "exec", containerID, "sh", "-c", command).CombinedOutput(); err != nil {
		t.Fatalf("catalog asset did not execute from a read-only mount: %v: %s", err, output)
	}
	inspect := exec.Command("docker", "inspect", "--format", `{{range .Mounts}}{{if eq .Destination "/opt/gpuworks/runtime"}}{{.RW}}{{end}}{{end}}`, containerID)
	output, err := inspect.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "false" {
		t.Fatalf("catalog asset mount is not read-only: output=%q err=%v", output, err)
	}
}

func deleteCatalogAssetDeployment(t *testing.T, env *testenv.Env, api *testenv.APIClient, deploymentID uint) {
	t.Helper()
	response, err := api.Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(deploymentID), 10))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		t.Fatalf("delete deployment status=%d", response.StatusCode)
	}
	env.Eventually(t, 30*time.Second, func() bool {
		instances, _ := env.Store().ListInstances()
		for _, instance := range instances {
			if instance.DeploymentID == deploymentID {
				return false
			}
		}
		return true
	})
}

func restoreWritableTree(root string) {
	_ = filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(name, 0o755)
		} else {
			_ = os.Chmod(name, 0o644)
		}
		return nil
	})
}
