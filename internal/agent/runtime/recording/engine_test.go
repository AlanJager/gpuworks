package recording

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// The agent asks after a container by its name; the driver answers for the
// id Run returned as well.
func TestStatusByNameOrID(t *testing.T) {
	d := New(filepath.Join(t.TempDir(), "launch.json"))
	id, err := d.Run(context.Background(), runtime.WorkloadSpec{Name: "gpuworks-abc", Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{id, "gpuworks-abc"} {
		st, _ := d.Status(context.Background(), key)
		if !st.Exists || !st.Running {
			t.Fatalf("status(%s) = %+v", key, st)
		}
	}
	if err := d.Stop(context.Background(), "gpuworks-abc"); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(context.Background(), id); !st.Exists || st.Running {
		t.Fatalf("stopped status = %+v", st)
	}
	if err := d.Remove(context.Background(), "gpuworks-abc"); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(context.Background(), id); st.Exists {
		t.Fatalf("removed status = %+v", st)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// A serving container gets the fake engine on the port its manual command
// names; a headless rank (port 0) gets none; stopping the container stops it.
func TestFakeEngineFollowsTheContainer(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-engine")
	build := exec.Command("go", "build", "-o", bin, "gpuworks.ai/gpuworks/test/fake-engine")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake engine: %v\n%s", err, out)
	}
	model := filepath.Join(dir, "model")
	if err := os.MkdirAll(model, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New(filepath.Join(dir, "launch.json")).WithEngine(bin)
	port := freePort(t)
	script := fmt.Sprintf("set -eu\nexec python3 -m sglang.launch_server --port %d --served-model-name 'glm-5.3'", port)
	spec := runtime.WorkloadSpec{Name: "gpuworks-leader", Image: "img", Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", script},
		Mounts: []runtime.Mount{{Source: model, Target: "/model", ReadOnly: true}}}
	if _, err := d.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	healthy := false
	for i := 0; i < 100 && !healthy; i++ {
		if resp, err := http.Get(url); err == nil {
			healthy = resp.StatusCode == http.StatusOK
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("the fake engine never answered on the container's port")
	}
	headless := spec
	headless.Name, headless.Cmd = "gpuworks-follower", []string{"-c", "exec python3 -m sglang.launch_server --port 0"}
	if _, err := d.Run(context.Background(), headless); err != nil {
		t.Fatal(err)
	}
	if len(d.engines) != 1 {
		t.Fatalf("engines = %d, want only the serving container's", len(d.engines))
	}
	if err := d.Stop(context.Background(), "gpuworks-leader"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := http.Get(url); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the fake engine outlived its container")
}
