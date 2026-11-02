package api

import (
	"context"
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

// Metadata parsing is an optional extension of register_path. Keeping path validation
// and metadata in one agent call avoids a second NFS traversal during preview.
func callNodeParseModelMetadata(ctx context.Context, call func(context.Context, string, string, any) (json.RawMessage, error), nodeUUID, path string, storageID uint) (registerPathResponse, error) {
	raw, err := call(ctx, nodeUUID, proto.RPCRegisterPath, map[string]any{"path": path, "inspect_metadata": true, "storage_id": storageID})
	if err != nil {
		return registerPathResponse{}, err
	}
	var out registerPathResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode model metadata: %w", err)
	}
	if out.Path == "" {
		out.Path = path
	}
	return out, nil
}

// Both import paths reject known invalid entries before writing to Store.
// Agents must identify one complete model entry in the selected directory.
// Older agents that cannot return recognition evidence must be upgraded.
func validateModelMetadata(report *modelmetadata.Report) error {
	if report == nil {
		return messages.NewProductError(messages.ErrorAgentUpgradeRequired, "upgrade the agent to validate model directory metadata before registration", nil)
	}
	return report.ValidateSingleEntry()
}
