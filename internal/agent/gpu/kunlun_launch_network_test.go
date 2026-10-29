package gpu

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
)

// Integrate the server's actual network resolution with native device injection.
// The network module owns verbs/limits; Kunlun adds only selected XPUs and XDR.
func TestKunlunConsumesResolvedHostRDMA(t *testing.T) {
	engine, err := deploy.SelectEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	for _, finite := range []bool{false, true} {
		params := deploy.Params{Vendor: "kunlunxin", ModelPath: "/model", AssignedGPUs: 1, TensorParallel: 2, NNodes: 2, MasterAddr: "192.0.2.1", MasterPort: 29500, ComputeNetwork: deploy.ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond0", HCAs: []string{"mlx5_1:1"}}}
		in := deploy.ResolveLaunchInput{Engine: engine, Params: params}
		if finite {
			in.ContainerOverrides.DockerArgs = []string{"--ulimit", "memlock=4096"}
		}
		launch, err := deploy.ResolveLaunch(in)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := kunlunFixture(t)
		withKunlunXDR(p, os.ModeDevice|os.ModeCharDevice)
		inj := p.RuntimeInjectionForLaunch(t.Context(), []int{1}, "Docker", 2, launch.DockerArgs)
		if finite {
			if !strings.Contains(inj.UnsupportedReason, "finite") || len(inj.DockerArgs) != 0 {
				t.Fatalf("finite limit accepted: %+v", inj)
			}
			continue
		}
		if inj.UnsupportedReason != "" {
			t.Fatal(inj.UnsupportedReason)
		}
		combined := append(append([]string(nil), launch.DockerArgs...), inj.DockerArgs...)
		for _, value := range []string{"/dev/infiniband", "memlock=-1", "IPC_LOCK", "/dev/xdrdrv", "/dev/xpu1:/dev/xpu1:rwm"} {
			count := 0
			for _, arg := range combined {
				if arg == value {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("%q appears %d times in %v", value, count, combined)
			}
		}
		if launch.Env["NCCL_IB_HCA"] != "mlx5_1:1" {
			t.Fatalf("selected rail changed: %v", launch.Env)
		}
	}
}

func TestKunlunFreshInjectionHonorsCallerCancellation(t *testing.T) {
	p, _ := kunlunFixture(t)
	p.command = func(ctx context.Context, _ ...string) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	inj := p.RuntimeInjectionForLaunch(ctx, []int{1}, "docker", 1, nil)
	if !strings.Contains(inj.UnsupportedReason, "deadline exceeded") || len(inj.DockerArgs) != 0 {
		t.Fatalf("cancelled injection: %+v", inj)
	}
}

func TestKunlunFreshExecutableRespectsDeadline(t *testing.T) {
	p, _ := kunlunFixture(t)
	exe := t.TempDir() + "/xpu-smi"
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	p.command = nil
	p.lookPath = func(string) (string, error) { return exe, nil }
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	inj := p.RuntimeInjectionForLaunch(ctx, []int{0}, "docker", 1, nil)
	if !strings.Contains(inj.UnsupportedReason, "deadline exceeded") || len(inj.DockerArgs) != 0 {
		t.Fatalf("deadline ignored: %+v", inj)
	}
}
