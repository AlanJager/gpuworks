package store

import (
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestUpdateDeploymentLaunchConfigWritesThreeColumnsAndPublishes(t *testing.T) {
	b := bus.New()
	s, err := Open(":memory:", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	dep, err := s.CreateDeployment(&Deployment{
		Name: "launch-config", ServedName: "launch-config", Engine: "vllm", State: DeployActive,
		QuickParams:              `{"max_model_len":4096}`,
		ExtraArgs:                "--max-model-len 8192 --foo",
		CatalogSelectionSnapshot: `{"spec_name":"balanced","extra_args":["--dtype","bfloat16"]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	sub := b.Subscribe(4, "deployment")
	defer sub.Close()

	const (
		quick    = `{"max_model_len":8192,"dtype":"bfloat16","gpu_memory_utilization":0.92}`
		extra    = "--foo"
		snapshot = `{"spec_name":"balanced","extra_args":[]}`
	)
	if err := s.UpdateDeploymentLaunchConfig(dep.ID, quick, extra, snapshot); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.QuickParams != quick || stored.ExtraArgs != extra || stored.CatalogSelectionSnapshot != snapshot {
		t.Fatalf("stored=%+v", stored)
	}
	select {
	case event := <-sub.C():
		if event.Key != uintKey(dep.ID) || event.Reason != "launch_config" {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("a launch configuration rewrite must publish a deployment event")
	}
}

func TestUpdateDeploymentLaunchConfigReportsAMissingDeployment(t *testing.T) {
	b := bus.New()
	s, err := Open(":memory:", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sub := b.Subscribe(1, "deployment")
	defer sub.Close()
	if err := s.UpdateDeploymentLaunchConfig(4242, `{}`, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
	select {
	case event := <-sub.C():
		t.Fatalf("a write that changed nothing must not publish: %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}
