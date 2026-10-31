package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// smokeRuntime is a container runtime as the check sees it: images that are
// or are not there, a container that exits with what the script printed, and
// every way a runtime call can fail.
type smokeRuntime struct {
	name      string
	images    map[string]bool
	imageErr  error
	runErr    error
	statusErr error
	hang      bool
	exitCode  int
	logs      string

	mu      sync.Mutex
	runs    []runtime.WorkloadSpec
	removed []string
	checked []string
}

func (r *smokeRuntime) Name() string                         { return r.name }
func (r *smokeRuntime) Detect(context.Context) (bool, error) { return true, nil }
func (r *smokeRuntime) PullImage(context.Context, string, func(string)) error {
	return errors.New("the runtime check must never pull")
}
func (r *smokeRuntime) Prepare(context.Context, runtime.WorkloadSpec) error    { return nil }
func (r *smokeRuntime) ListContainers(context.Context) ([]string, error)       { return nil, nil }
func (r *smokeRuntime) RunningContainers(context.Context) ([]string, error)    { return nil, nil }
func (r *smokeRuntime) Stop(context.Context, string) error                     { return nil }
func (r *smokeRuntime) Exec(context.Context, string, []string) ([]byte, error) { return nil, nil }
func (r *smokeRuntime) ImageExists(_ context.Context, image string) (bool, error) {
	r.mu.Lock()
	r.checked = append(r.checked, image)
	r.mu.Unlock()
	return r.images[image], r.imageErr
}
func (r *smokeRuntime) Run(_ context.Context, spec runtime.WorkloadSpec) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, spec)
	if r.runErr != nil {
		return "", r.runErr
	}
	return "id-" + spec.Name, nil
}
func (r *smokeRuntime) Remove(_ context.Context, id string) error {
	r.mu.Lock()
	r.removed = append(r.removed, id)
	r.mu.Unlock()
	return nil
}
func (r *smokeRuntime) Status(context.Context, string) (runtime.Status, error) {
	if r.statusErr != nil {
		return runtime.Status{}, r.statusErr
	}
	return runtime.Status{Exists: true, Running: r.hang, ExitCode: r.exitCode}, nil
}
func (r *smokeRuntime) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(r.logs)), nil
}

// smokeBuilder injects the card the way the engine does for PPU on docker.
type smokeBuilder struct{ err error }

func (b smokeBuilder) BuildCardSmokeWorkload(image string, index int, name string, entrypoint []string) (runtime.WorkloadSpec, error) {
	if b.err != nil {
		return runtime.WorkloadSpec{}, b.err
	}
	return runtime.WorkloadSpec{Name: name, Image: image, GPUIndexes: []int{index}, Entrypoint: entrypoint,
		Env: map[string]string{accelerator.EnvCUDAVisibleDevices: "0"}}, nil
}

const ppuImage = "swr.example/zs/gpuworks-runner:hggc13.0-sglang0.5.13-r6"

func newPPUSmoke(r *smokeRuntime, images ...string) *containerSmoke {
	return &containerSmoke{
		vendor: accelerator.VendorPPU, framework: "hggc",
		driver: func() runtime.Driver { return r }, builder: smokeBuilder{},
		images: func() []string { return images },
	}
}

func TestContainerSmokePicksALocalRunnerImageOfTheNodesFramework(t *testing.T) {
	r := &smokeRuntime{name: "docker", images: map[string]bool{
		"gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r1": true, ppuImage: true,
	}}
	c := newPPUSmoke(r, "my/custom:latest", "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r1",
		"gpuworks/gpuworks-runner:hggc13.0-vllm0.23.0-r3", ppuImage, ppuImage)
	if reason := c.pick(t.Context()); reason != "" {
		t.Fatalf("pick = %q", reason)
	}
	if c.chosen() != ppuImage {
		t.Fatalf("chose %q", c.chosen())
	}
	// Only refs of the node's framework are asked about, each once.
	if strings.Join(r.checked, ",") != "gpuworks/gpuworks-runner:hggc13.0-vllm0.23.0-r3,"+ppuImage {
		t.Fatalf("asked about %v", r.checked)
	}
}

func TestContainerSmokeSaysWhyItCannotRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		driver runtime.Driver
		images []string
		want   string
	}{
		{"no runtime", nil, []string{ppuImage}, "no container runtime"},
		{"udocker", &smokeRuntime{name: "udocker", images: map[string]bool{ppuImage: true}}, []string{ppuImage}, "not run on udocker nodes"},
		{"no image", &smokeRuntime{name: "docker", images: map[string]bool{}}, []string{ppuImage}, "no hggc runner image is on this node"},
		{"inspect fails", &smokeRuntime{name: "nerdctl", images: map[string]bool{ppuImage: true}, imageErr: errors.New("daemon down")}, []string{ppuImage}, "no hggc runner image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &containerSmoke{vendor: "ppu", framework: "hggc", builder: smokeBuilder{},
				driver: func() runtime.Driver { return tc.driver }, images: func() []string { return tc.images }}
			if reason := c.pick(t.Context()); !strings.Contains(reason, tc.want) {
				t.Fatalf("pick = %q, want %q", reason, tc.want)
			}
		})
	}
}

func TestContainerSmokeGradesTheCardApartFromTheContainer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		r       *smokeRuntime
		builder smokeBuilder
		ok      bool
		onHost  bool
		want    string
	}{
		{name: "passes", r: &smokeRuntime{logs: "CUDA_RUNTIME_SMOKE_OK\n"}, ok: true},
		{name: "card fails", r: &smokeRuntime{exitCode: 1, logs: "CUDA_RUNTIME_SMOKE_CARD_FAILED: cuMemAlloc=2\n"}, want: "cuMemAlloc=2"},
		{name: "runtime missing in image", r: &smokeRuntime{exitCode: 10, logs: "CUDA_RUNTIME_SMOKE_UNAVAILABLE: cuInit=100\n"}, onHost: true, want: "cuInit=100"},
		{name: "no python in image", r: &smokeRuntime{exitCode: 127, logs: "exec: python3: not found\n"}, onHost: true, want: "python3"},
		{name: "run fails", r: &smokeRuntime{runErr: errors.New("device not found")}, onHost: true, want: "did not start"},
		{name: "inspect fails", r: &smokeRuntime{statusErr: errors.New("no such container")}, onHost: true, want: "could not be inspected"},
		{name: "injection fails", r: &smokeRuntime{}, builder: smokeBuilder{err: errors.New("PPU device 2 is not present")}, onHost: true, want: "could not be built"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.r
			r.name, r.images = "docker", map[string]bool{ppuImage: true}
			c := newPPUSmoke(r, ppuImage)
			c.builder = tc.builder
			if reason := c.pick(t.Context()); reason != "" {
				t.Fatal(reason)
			}
			err := c.try(t.Context(), "2")
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if !tc.ok {
				if got := gpu.HostRuntimeUnavailable(err) != ""; got != tc.onHost {
					t.Fatalf("host-level = %v, want %v (%v)", got, tc.onHost, err)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want %q", err, tc.want)
				}
			}
			if len(r.runs) == 0 {
				return
			}
			run := r.runs[0]
			if run.Image != ppuImage || len(run.GPUIndexes) != 1 || run.GPUIndexes[0] != 2 ||
				len(run.Entrypoint) != 3 || run.Entrypoint[0] != "python3" || run.Entrypoint[2] != accelerator.CUDAGradedSmokeScript ||
				!strings.HasPrefix(run.Name, "gpuworks-card-smoke-2-") {
				t.Fatalf("workload = %+v", run)
			}
			if tc.r.runErr == nil && (len(r.removed) != 1 || r.removed[0] != "id-"+run.Name) {
				t.Fatalf("removed %v, want the container gone after the check", r.removed)
			}
		})
	}
}

func TestContainerSmokeHangIsLeftToTheRunnersDeadline(t *testing.T) {
	r := &smokeRuntime{name: "docker", images: map[string]bool{ppuImage: true}, hang: true}
	c := newPPUSmoke(r, ppuImage)
	if reason := c.pick(t.Context()); reason != "" {
		t.Fatal(reason)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := c.try(ctx, "2")
	if !errors.Is(err, context.DeadlineExceeded) || gpu.HostRuntimeUnavailable(err) != "" {
		t.Fatalf("err = %v, want the deadline (the card did not answer)", err)
	}
	if len(r.removed) != 1 {
		t.Fatalf("removed %v, want the hung container removed", r.removed)
	}
}

func TestComposedCheckPrefersTheHostRuntime(t *testing.T) {
	r := &smokeRuntime{name: "docker", images: map[string]bool{ppuImage: true}, logs: "CUDA_RUNTIME_SMOKE_OK\n"}
	hostReason := ""
	var hostAsked []string
	host := gpu.HostRuntimeCheck{
		CLI: "npu-smi", Timeout: 45 * time.Second,
		Prerequisite: func() string { return hostReason },
		Visible:      func(g proto.GPUInfo) (string, bool) { return "host-" + g.UUID, true },
		Try:          func(_ context.Context, v string) error { hostAsked = append(hostAsked, v); return nil },
	}
	check := composeCardRuntimeCheck("ascend", host, true, newPPUSmoke(r, ppuImage))
	if check.CLI != "npu-smi" || check.Timeout != containerSmokeStartBudget+containerSmokeScriptBudget {
		t.Fatalf("check = %+v", check)
	}
	if reason := check.Prerequisite(); reason != "" {
		t.Fatal(reason)
	}
	if err := check.Try(t.Context(), "3"); err != nil || len(hostAsked) != 1 || len(r.runs) != 0 {
		t.Fatalf("host runtime present: err=%v host=%v container runs=%d", err, hostAsked, len(r.runs))
	}

	hostReason = "the Ascend runtime check needs libascendcl.so from a host CANN installation"
	if reason := check.Prerequisite(); reason != "" {
		t.Fatal(reason)
	}
	if err := check.Try(t.Context(), "3"); err != nil || len(r.runs) != 1 {
		t.Fatalf("driver-only host: err=%v container runs=%d", err, len(r.runs))
	}

	r.images = map[string]bool{}
	if reason := check.Prerequisite(); !strings.Contains(reason, "libascendcl") || !strings.Contains(reason, "no hggc runner image") {
		t.Fatalf("neither: reason = %q, want both halves", reason)
	}
}

func TestComposedCheckForAVendorWithoutAHostRuntime(t *testing.T) {
	check := composeCardRuntimeCheck("ppu", gpu.HostRuntimeCheck{}, false, newPPUSmoke(&smokeRuntime{name: "docker"}, ppuImage))
	if check.CLI != "ppu-smi" {
		t.Fatalf("CLI = %q", check.CLI)
	}
	if v, ok := check.Visible(proto.GPUInfo{Index: 7}); !ok || v != "7" {
		t.Fatalf("Visible = %q %v", v, ok)
	}
	if _, ok := check.Visible(proto.GPUInfo{Index: -1}); ok {
		t.Fatal("a card without a device index was named")
	}
}
