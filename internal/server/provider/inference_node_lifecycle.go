package provider

import (
	"context"
	"fmt"
)

// InferenceNodeLifecycleHandler adds platform-specific work around an
// inference instance without changing the ordinary node lifecycle.
type InferenceNodeLifecycleHandler interface {
	BeforeCreate(context.Context, InferenceNodeCreateRequest) error
	OnCreateFailed(context.Context, InferenceNodeCreateRequest) error
	RequiresStopConfirmation(context.Context, string) (bool, error)
	AfterInstanceTerminal(context.Context, InferenceNodeStopRequest) error
}

// InferenceNodeLifecycleProvider optionally supplies a handler for a node.
// Ownership lookup must be read-only. Rules stay in the driver; nil means this
// node is not managed by it, and an error prevents the lifecycle operation.
type InferenceNodeLifecycleProvider interface {
	InferenceNodeLifecycleFor(nodeUUID string) (InferenceNodeLifecycleHandler, error)
}

// InferenceNodeCreateRequest is the generic input passed to platform-specific
// lifecycle handlers. The controller does not decide which platform owns the
// node; the handler decides whether the request applies to it.
type InferenceNodeCreateRequest struct {
	NodeUUID     string
	InstanceUUID string
	GPUIndexes   []int
}

type InferenceNodeStopRequest struct {
	NodeUUID     string
	InstanceUUID string
	// State and Message carry the terminal agent report that authorized the
	// external-resource release. A failed teardown is intentionally distinguishable
	// from an ordinary failed startup: the former may still hold resources.
	State   string
	Message string
}

func (s *Service) inferenceNodeLifecycleFor(nodeUUID string) (InferenceNodeLifecycleHandler, error) {
	var handler InferenceNodeLifecycleHandler
	for _, name := range s.configuredDriverNames() {
		resolver, ok := s.drivers[name].(InferenceNodeLifecycleProvider)
		if !ok {
			continue
		}
		candidate, err := resolver.InferenceNodeLifecycleFor(nodeUUID)
		if err != nil {
			return nil, fmt.Errorf("provider %s node lifecycle: %w", name, err)
		}
		if candidate == nil {
			continue
		}
		if handler != nil {
			return nil, fmt.Errorf("multiple providers claim inference node %s", nodeUUID)
		}
		handler = candidate
	}
	return handler, nil
}

func (s *Service) BeforeCreate(ctx context.Context, req InferenceNodeCreateRequest) error {
	handler, err := s.inferenceNodeLifecycleFor(req.NodeUUID)
	if err != nil || handler == nil {
		return err
	}
	return handler.BeforeCreate(ctx, req)
}

func (s *Service) OnCreateFailed(ctx context.Context, req InferenceNodeCreateRequest) error {
	handler, err := s.inferenceNodeLifecycleFor(req.NodeUUID)
	if err != nil || handler == nil {
		return err
	}
	return handler.OnCreateFailed(ctx, req)
}

func (s *Service) RequiresStopConfirmation(ctx context.Context, nodeUUID string) (bool, error) {
	handler, err := s.inferenceNodeLifecycleFor(nodeUUID)
	if err != nil || handler == nil {
		return false, err
	}
	return handler.RequiresStopConfirmation(ctx, nodeUUID)
}

func (s *Service) AfterInstanceTerminal(ctx context.Context, req InferenceNodeStopRequest) error {
	handler, err := s.inferenceNodeLifecycleFor(req.NodeUUID)
	if err != nil || handler == nil {
		return err
	}
	return handler.AfterInstanceTerminal(ctx, req)
}
