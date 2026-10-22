package provider

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type lifecycleTestDriver struct {
	serviceTestDriver
	name       string
	node       string
	handler    *lifecycleTestHandler
	resolveErr error
}

func (d *lifecycleTestDriver) Name() string { return d.name }

func (d *lifecycleTestDriver) InferenceNodeLifecycleFor(nodeUUID string) (InferenceNodeLifecycleHandler, error) {
	if d.resolveErr != nil {
		return nil, d.resolveErr
	}
	if nodeUUID != d.node {
		return nil, nil
	}
	return d.handler, nil
}

type lifecycleTestHandler struct {
	create  InferenceNodeCreateRequest
	failed  InferenceNodeCreateRequest
	stopped InferenceNodeStopRequest
	waitFor string
	err     error
}

func (h *lifecycleTestHandler) BeforeCreate(_ context.Context, req InferenceNodeCreateRequest) error {
	h.create = req
	return h.err
}

func (h *lifecycleTestHandler) OnCreateFailed(_ context.Context, req InferenceNodeCreateRequest) error {
	h.failed = req
	return h.err
}

func (h *lifecycleTestHandler) RequiresStopConfirmation(_ context.Context, node string) (bool, error) {
	h.waitFor = node
	return true, h.err
}

func (h *lifecycleTestHandler) AfterInstanceTerminal(_ context.Context, req InferenceNodeStopRequest) error {
	h.stopped = req
	return h.err
}

func TestServiceDispatchesNodeLifecycleOnlyToItsOwner(t *testing.T) {
	svc := NewService(nil, nil)
	owner, other := &lifecycleTestHandler{}, &lifecycleTestHandler{}
	svc.SetDriver(&lifecycleTestDriver{name: "first", node: "managed", handler: owner})
	svc.SetDriver(&lifecycleTestDriver{name: "second", node: "different", handler: other})
	svc.SetDriver(serviceTestDriver{})
	for _, node := range []string{"ordinary", "managed"} {
		req := InferenceNodeCreateRequest{NodeUUID: node, InstanceUUID: "instance", GPUIndexes: []int{1, 3}}
		stop := InferenceNodeStopRequest{NodeUUID: node, InstanceUUID: "instance", State: "failed", Message: "teardown failed: still running"}
		if err := svc.BeforeCreate(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		if err := svc.OnCreateFailed(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		wait, err := svc.RequiresStopConfirmation(t.Context(), node)
		if err != nil || wait != (node == "managed") {
			t.Fatalf("stop confirmation for %s = %v, %v", node, wait, err)
		}
		if err := svc.AfterInstanceTerminal(t.Context(), stop); err != nil {
			t.Fatal(err)
		}
		if node == "managed" {
			if !reflect.DeepEqual(owner.create, req) || !reflect.DeepEqual(owner.failed, req) || owner.stopped != stop || owner.waitFor != node {
				t.Fatalf("lifecycle input was lost: %+v", owner)
			}
		} else if !reflect.DeepEqual(owner, &lifecycleTestHandler{}) {
			t.Fatal("ordinary node reached a platform handler")
		}
	}
	if !reflect.DeepEqual(other, &lifecycleTestHandler{}) {
		t.Fatal("another platform's handler received this node's lifecycle")
	}
}

func TestServiceLifecyclePreservesErrors(t *testing.T) {
	for _, failResolution := range []bool{false, true} {
		sentinel := errors.New("platform unavailable")
		handler := &lifecycleTestHandler{err: sentinel}
		driver := &lifecycleTestDriver{name: "error-platform", node: "managed", handler: handler}
		if failResolution {
			driver.resolveErr = sentinel
		}
		svc := NewService(nil, nil)
		svc.SetDriver(driver)
		req := InferenceNodeCreateRequest{NodeUUID: "managed"}
		_, waitErr := svc.RequiresStopConfirmation(t.Context(), "managed")
		for _, err := range []error{
			svc.BeforeCreate(t.Context(), req), svc.OnCreateFailed(t.Context(), req), waitErr,
			svc.AfterInstanceTerminal(t.Context(), InferenceNodeStopRequest{NodeUUID: "managed"}),
		} {
			if !errors.Is(err, sentinel) {
				t.Fatalf("lost platform error: %v", err)
			}
		}
		if failResolution && handler.waitFor != "" {
			t.Fatal("handler ran after ownership lookup failed")
		}
	}
}

func TestServiceRejectsAmbiguousOwnershipBeforeResourceChanges(t *testing.T) {
	svc := NewService(nil, nil)
	first, second := &lifecycleTestHandler{}, &lifecycleTestHandler{}
	svc.SetDriver(&lifecycleTestDriver{name: "first", node: "same", handler: first})
	svc.SetDriver(&lifecycleTestDriver{name: "second", node: "same", handler: second})
	if err := svc.BeforeCreate(t.Context(), InferenceNodeCreateRequest{NodeUUID: "same"}); err == nil {
		t.Fatal("two owners must not both mutate resources")
	}
	if first.create.NodeUUID != "" || second.create.NodeUUID != "" {
		t.Fatal("ownership conflict was detected after resources changed")
	}
}
