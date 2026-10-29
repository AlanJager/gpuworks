package engine

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

type topologyInjectionProber struct {
	injectionProber
	indexes []int
	driver  string
	nodes   int
	fail    bool
}

func (p *topologyInjectionProber) RuntimeInjectionForLaunch(_ context.Context, indexes []int, driver string, nodes int, _ []string) gpu.RuntimeInjection {
	p.indexes, p.driver, p.nodes = append([]int(nil), indexes...), driver, nodes
	if p.fail {
		return gpu.RuntimeInjection{UnsupportedReason: "transport unavailable"}
	}
	return gpu.RuntimeInjection{DockerArgs: []string{"--device", "/dev/example-transport"}}
}

func TestDeviceInjectionUsesTopologyExtensionWithoutVendorBranch(t *testing.T) {
	p := &topologyInjectionProber{}
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), p)
	ws := runtime.WorkloadSpec{DockerArgs: []string{"--ipc=host"}}
	if err := e.applyDeviceInjectionForLaunch(context.Background(), &ws, []int{3, 7}, "docker", 2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.indexes, []int{3, 7}) || p.driver != "docker" || p.nodes != 2 || !reflect.DeepEqual(ws.DockerArgs, []string{"--ipc=host", "--device", "/dev/example-transport"}) {
		t.Fatalf("p=%+v ws=%+v", p, ws)
	}
	p.fail = true
	before := append([]string(nil), ws.DockerArgs...)
	if err := e.applyDeviceInjectionForLaunch(context.Background(), &ws, []int{3}, "docker", 2); err == nil || !reflect.DeepEqual(ws.DockerArgs, before) {
		t.Fatalf("failure changed workload: %+v, %v", ws, err)
	}
}

func TestDeviceInjectionAllowsEmptySelectionForBackendsThatSupportIt(t *testing.T) {
	// Preserve the existing engine-level no-injection contract, including an
	// optional topology extension that would reject or inject if called.
	p := &topologyInjectionProber{fail: true}
	withExtension := NewWithProber(&headlessTestDriver{}, nil, slog.Default(), t.TempDir(), p)
	ws := runtime.WorkloadSpec{DockerArgs: []string{"--ipc=host"}}
	if err := withExtension.applyDeviceInjectionForLaunch(context.Background(), &ws, nil, "docker", 2); err != nil || len(ws.DockerArgs) != 1 || p.nodes != 0 {
		t.Fatalf("empty selection reached extension: %+v %+v %v", p, ws, err)
	}
	e := NewWithProber(&headlessTestDriver{}, nil, slog.Default(), t.TempDir(), gpu.NoAccelerator{})
	if err := e.applyDeviceInjectionForLaunch(context.Background(), &runtime.WorkloadSpec{}, nil, "docker", 1); err != nil {
		t.Fatal(err)
	}
}

// An injector implementing all optional seams must receive the resolved args
// through the richest interface exactly once.
type allInjectionProber struct {
	topologyInjectionProber
	called string
	args   []string
}

func (p *allInjectionProber) RuntimeInjectionForDriver([]int, string) gpu.RuntimeInjection {
	p.called = "driver"
	return gpu.RuntimeInjection{UnsupportedReason: "wrong interface"}
}
func (p *allInjectionProber) RuntimeInjectionForLaunch(ctx context.Context, indexes []int, driver string, nodes int, args []string) gpu.RuntimeInjection {
	p.called = "resolved"
	p.args = append([]string(nil), args...)
	if err := ctx.Err(); err != nil {
		return gpu.RuntimeInjection{UnsupportedReason: err.Error()}
	}
	return gpu.RuntimeInjection{DockerArgs: []string{"--device", "/dev/xdrdrv"}}
}
func TestResolvedInjectionPrecedesOtherOptionalInterfaces(t *testing.T) {
	p := &allInjectionProber{}
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), p)
	ws := runtime.WorkloadSpec{DockerArgs: []string{"--device", "/dev/infiniband"}, Env: map[string]string{}}
	if err := e.applyDeviceInjectionForLaunch(context.Background(), &ws, []int{1}, "docker", 2); err != nil {
		t.Fatal(err)
	}
	if p.called != "resolved" || !reflect.DeepEqual(p.args, []string{"--device", "/dev/infiniband"}) || p.nodes != 0 {
		t.Fatalf("dispatch: %+v", p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := append([]string(nil), ws.DockerArgs...)
	if err := e.applyDeviceInjectionForLaunch(ctx, &ws, []int{1}, "docker", 2); err == nil || !reflect.DeepEqual(ws.DockerArgs, before) {
		t.Fatalf("cancel mutated workload: %v %+v", err, ws)
	}
}
