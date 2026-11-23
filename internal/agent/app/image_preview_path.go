package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
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
		return nil, archivePreviewError(ctx, err, proto.ArchivePreviewUnreadable)
	}
	defer file.Close()
	return readNodeArchivePreview(ctx, file)
}

func readNodeArchivePreview(ctx context.Context, file io.ReadSeeker) (any, error) {
	contents, err := imagearchive.ReadContents(ctx, file)
	if err != nil {
		return nil, archivePreviewError(ctx, err, proto.ArchivePreviewInvalid)
	}
	if contents.RepoTags == nil {
		contents.RepoTags = []string{}
	}
	result := proto.ImagePreviewPathResult{RepoTags: contents.RepoTags}
	if len(contents.MetadataByTag) > 0 {
		result.MetadataByTag = make(map[string]proto.ArchiveImageMetadata, len(contents.MetadataByTag))
		for tag, metadata := range contents.MetadataByTag {
			result.MetadataByTag[tag] = proto.ArchiveImageMetadata(metadata)
		}
	}
	return result, nil
}

func archivePreviewError(ctx context.Context, err error, reason string) error {
	reason = archiveFileErrorReason(ctx, err, reason)
	return &proto.RPCError{Code: proto.ErrorArchivePreview, Message: "node archive preview failed", Params: map[string]any{"reason": reason}}
}

func archiveFileErrorReason(ctx context.Context, err error, reason string) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		reason = proto.ArchivePreviewTimeout
	case errors.Is(err, fs.ErrNotExist):
		reason = proto.ArchivePreviewNotFound
	case errors.Is(err, fs.ErrPermission):
		reason = proto.ArchivePreviewPermission
	case errors.Is(err, runtime.ErrImageFileNotRegular):
		reason = proto.ArchivePreviewNotRegular
	}
	return reason
}
