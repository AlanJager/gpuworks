//go:build darwin

package hostproc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// TestEnvironmentPackageServesOnThisMac runs a real engine from a package
// built by build/host-envs on this Mac's GPU. It is opt-in: it needs the
// package archive and a model directory, and it loads the model.
//
//	GPUWORKS_HOSTENV_ARCHIVE=vllm-metal.tar GPUWORKS_HOSTENV_IMAGE=gpuworks/vllm-metal:0.31.0-darwin-arm64 \
//	GPUWORKS_HOSTENV_MODEL=/path/to/Qwen3-0.6B-4bit go test -run EnvironmentPackageServes ./internal/agent/runtime/hostproc
func TestEnvironmentPackageServesOnThisMac(t *testing.T) {
	archive, image, model := os.Getenv("GPUWORKS_HOSTENV_ARCHIVE"), os.Getenv("GPUWORKS_HOSTENV_IMAGE"), os.Getenv("GPUWORKS_HOSTENV_MODEL")
	if archive == "" || image == "" || model == "" {
		t.Skip("GPUWORKS_HOSTENV_ARCHIVE, GPUWORKS_HOSTENV_IMAGE and GPUWORKS_HOSTENV_MODEL are not set")
	}
	state := os.Getenv("GPUWORKS_HOSTENV_STATE")
	if state == "" {
		state = t.TempDir()
	}
	d := New(state)
	ctx := context.Background()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := d.LoadImage(ctx, image, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Logf("loaded %s in %s", image, time.Since(began).Round(time.Millisecond))

	const port = "18190"
	spec := runtime.WorkloadSpec{
		Name:       "gpuworks-hostenv-smoke",
		Image:      image,
		Entrypoint: []string{"vllm", "serve", "/model"},
		Cmd:        []string{"--host", "127.0.0.1", "--port", port, "--served-model-name", "m", "--max-model-len", "4096", "--gpu-memory-utilization", "0.1"},
		Env:        map[string]string{"HF_HUB_OFFLINE": "1"},
		Mounts:     []runtime.Mount{{Source: model, Target: "/model", ReadOnly: true}},
	}
	began = time.Now()
	id, err := d.Run(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("unpacked and started in %s", time.Since(began).Round(time.Millisecond))
	defer func() {
		if err := d.Remove(context.Background(), id); err != nil {
			t.Errorf("Remove: %v", err)
		}
		if err := d.RemoveImage(context.Background(), image); err != nil {
			t.Errorf("RemoveImage: %v", err)
		}
	}()
	dumpLogs := func() {
		if rc, err := d.Logs(ctx, id, false, 60); err == nil {
			data, _ := io.ReadAll(rc)
			rc.Close()
			t.Logf("engine log tail:\n%s", data)
		}
	}
	base := "http://127.0.0.1:" + port
	healthy := false
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if st, _ := d.Status(ctx, id); !st.Running {
			dumpLogs()
			t.Fatalf("engine exited: %+v", st)
		}
		if resp, err := http.Get(base + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				healthy = true
				break
			}
		}
	}
	if !healthy {
		dumpLogs()
		t.Fatal("engine never became healthy")
	}
	t.Logf("healthy %s after start", time.Since(began).Round(time.Second))

	body := `{"model":"m","prompt":"The capital of France is","max_tokens":32,"temperature":0}`
	began = time.Now()
	resp, err := http.Post(base+"/v1/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var completion struct {
		Choices []struct{ Text string } `json:"choices"`
		Usage   struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	err = json.NewDecoder(resp.Body).Decode(&completion)
	resp.Body.Close()
	if err != nil || len(completion.Choices) == 0 || completion.Usage.CompletionTokens == 0 {
		t.Fatalf("completion = %+v, %v", completion, err)
	}
	t.Logf("completion (%d tokens in %s): %q", completion.Usage.CompletionTokens, time.Since(began).Round(time.Millisecond), completion.Choices[0].Text)

	resp, err = http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(metrics), "vllm:generation_tokens_total") {
		t.Fatalf("metrics lack vllm:generation_tokens_total")
	}

	if err := d.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if st := waitStopped(t, d, id); st.Exists && st.Running {
		t.Fatalf("still running after Stop: %+v", st)
	}
}
