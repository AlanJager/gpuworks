package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func (h *Hub) ImageExists(ctx context.Context, ref, nodeUUID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := h.CallNode(ctx, nodeUUID, proto.RPCImageExists, proto.ImageExistsParams{ImageRef: ref})
	if err != nil {
		return false, err
	}
	var result struct {
		Exists *bool `json:"exists"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return false, err
	}
	if result.Exists == nil {
		return false, fmt.Errorf("image.exists response missing exists")
	}
	return *result.Exists, nil
}
