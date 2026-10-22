package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// rpcImageRemove deletes one image from this node.
//
// A runtime that still needs the image — a container is running from it —
// refuses, and that refusal is returned as-is: the operator has to stop what is
// using it rather than have the node quietly force the deletion.
func (n *NodeRuntime) rpcImageRemove(ctx context.Context, raw json.RawMessage) (any, error) {
	var p proto.ImageRemoveParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	p.ImageRef = strings.TrimSpace(p.ImageRef)
	if p.ImageRef == "" {
		return nil, errors.New("image remove requires image_ref")
	}
	d := n.runtimeDriver()
	if d == nil {
		return nil, errors.New("runtime driver is unavailable")
	}
	remover, ok := d.(runtime.ImageRemover)
	if !ok {
		return nil, fmt.Errorf("runtime %s cannot remove images", n.RuntimeDriverName())
	}
	if err := remover.RemoveImage(ctx, p.ImageRef); err != nil {
		return nil, err
	}
	n.reportImageNode(proto.ImageNodeStatus{
		ImageRef: p.ImageRef, State: "absent", Message: "image removed from this node",
	})
	return proto.ImageRemoveResult{State: "absent", ImageRef: p.ImageRef}, nil
}
