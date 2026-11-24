package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/proto"
)

func (i *nodeImages) rpcImageInspect(ctx context.Context, raw json.RawMessage) (any, error) {
	var p proto.ImageInspectParams
	if err := json.Unmarshal(raw, &p); err != nil || strings.TrimSpace(p.ImageRef) == "" {
		return nil, errors.New("image inspect requires image_ref")
	}
	d := i.deps.driver()
	inspector, ok := d.(runtime.ImageInspector)
	if !ok {
		return nil, errors.New("runtime does not support image inspection")
	}
	inspected, err := inspector.InspectImage(ctx, strings.TrimSpace(p.ImageRef))
	if err != nil {
		return nil, err
	}
	return proto.ImageInspectResult{ImageRef: strings.TrimSpace(p.ImageRef), ImageID: inspected.ID, Entrypoint: inspected.Entrypoint, Cmd: inspected.Cmd, Env: commandlog.RedactEnv(inspected.Env), Volumes: inspected.Volumes}, nil
}
