package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

func (n *NodeRuntime) rpcImageExists(ctx context.Context, raw json.RawMessage) (any, error) {
	var p proto.ImageExistsParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	ref := strings.TrimSpace(p.ImageRef)
	if ref == "" {
		return nil, errors.New("image.exists requires image_ref")
	}
	d := n.runtimeDriver()
	if d == nil {
		return nil, errors.New("runtime unavailable")
	}
	exists, err := d.ImageExists(ctx, ref)
	if err != nil {
		return nil, err
	}
	return proto.ImageExistsResult{Exists: exists}, nil
}
