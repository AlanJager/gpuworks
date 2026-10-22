package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// unavailableRuntime is the error a driver whose setup failed returns from
// every call, as the udocker driver does on a volume that refuses chown.
func unavailableRuntime() error {
	return &runtime.UnavailableError{Runtime: "udocker", Err: fmt.Errorf("prepare udocker ownership: chown -R udocker /workspace/udocker: %w", syscall.EPERM)}
}

// A runtime that cannot be set up fails every source alike, so the pull loop
// stops at the first and says the runtime is at fault instead of listing each
// registry as if it had refused the image.
func TestRuntimeProvisioningFailureStopsTheSourceLoopAndIsNotBlamedOnTheSource(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}, pullErr: unavailableRuntime()}
	statuses := startImageSpec(t, driver, proto.InstanceDesiredState{
		Image: "lmsysorg/sglang:latest", ImagePullRefs: []string{"lmsysorg/sglang:latest", "swr.example.com/lmsysorg/sglang:latest"},
	})
	msg := failedMessage(statuses)
	want := "udocker runtime unavailable: prepare udocker ownership: chown -R udocker /workspace/udocker: operation not permitted"
	if msg != want {
		t.Fatalf("failure = %q, want %q", msg, want)
	}
	if strings.Contains(msg, "all image sources failed") {
		t.Fatalf("the failure blames the image sources: %q", msg)
	}
	if len(driver.pullRefs) != 1 {
		t.Fatalf("pulled %v, want the loop to stop after the first source", driver.pullRefs)
	}
	if driver.runCalls != 0 {
		t.Fatalf("run called %d times after the runtime failed", driver.runCalls)
	}
}

// A registry failure is still blamed on the source and moves on to the next.
func TestOrdinaryPullFailureStillTriesTheNextSource(t *testing.T) {
	driver := &imageToleranceDriver{present: map[string]bool{}, pullErrs: map[string]error{"a.example/img:1": errors.New("registry down")}}
	statuses := startImageSpec(t, driver, proto.InstanceDesiredState{
		Image: "a.example/img:1", ImagePullRefs: []string{"a.example/img:1", "b.example/img:1"},
	})
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if len(driver.pullRefs) != 2 {
		t.Fatalf("pulled %v, want both sources", driver.pullRefs)
	}
}

// unavailableOnCheck fails the presence check the way a driver does before
// any pull is attempted.
type unavailableOnCheck struct{ imageToleranceDriver }

func (d *unavailableOnCheck) ImageExists(context.Context, string) (bool, error) {
	return false, unavailableRuntime()
}

func TestRuntimeProvisioningFailureInTheImageCheckSkipsTheDownload(t *testing.T) {
	driver := &unavailableOnCheck{imageToleranceDriver{present: map[string]bool{}}}
	statuses := startImageSpec(t, driver, proto.InstanceDesiredState{Image: "repo:v1"})
	if msg := failedMessage(statuses); !strings.HasPrefix(msg, "udocker runtime unavailable: ") {
		t.Fatalf("failure = %q", msg)
	}
	if driver.pullCalls != 0 || driver.runCalls != 0 {
		t.Fatalf("pulled %d and ran %d times with an unusable runtime", driver.pullCalls, driver.runCalls)
	}
}
