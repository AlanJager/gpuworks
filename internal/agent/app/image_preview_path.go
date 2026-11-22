package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/imagearchive"
)

func (n *NodeRuntime) rpcImagePreviewPath(ctx context.Context, raw json.RawMessage) (any, error) {
	var req proto.ImagePreviewPathParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimSpace(req.ArchivePath)
	if !filepath.IsAbs(path) {
		return nil, errors.New("archive_path must be an absolute file path on the target node")
	}
	ctx, cancel := context.WithTimeout(ctx, proto.ImageArchivePreviewTimeout)
	defer cancel()
	file, err := runtime.OpenImageFile(ctx, n.runtimeDriver(), path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, runtime.ErrImageFileNotRegular
	}
	tags, err := imagearchive.ReadRepoTags(ctx, file)
	if err != nil {
		return nil, err
	}
	if tags == nil {
		tags = []string{}
	}
	return proto.ImagePreviewPathResult{RepoTags: tags}, nil
}
