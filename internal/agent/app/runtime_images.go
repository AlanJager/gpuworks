package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/imagearchive"
)

// runtimeImageHandlers is the RPCs over images the node's runtimes already
// hold. They refuse with ErrorLocalImageBusy while the image slot is taken.
var runtimeImageHandlers = map[string]func(*nodeImages, context.Context, string, json.RawMessage) (any, error){
	proto.RPCImageRuntimeList:    (*nodeImages).localSelection,
	proto.RPCImageRuntimeInspect: (*nodeImages).localSelection,
	proto.RPCImageRuntimeImport:  (*nodeImages).localSelection,
	proto.RPCImageRuntimePrepare: (*nodeImages).localSelection,
	proto.RPCImageRuntimeCopy:    (*nodeImages).startLocalCopy,
}

func (i *nodeImages) localSelection(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var selection proto.RuntimeImageSelection
	if err := json.Unmarshal(raw, &selection); err != nil {
		return nil, err
	}
	d, ok := i.deps.driver().(runtime.LocalImageStore)
	if !ok {
		return nil, localImageError(method, selection, runtime.ErrLocalImageUnsupported)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if method == proto.RPCImageRuntimeList {
		result, err := d.ListLocalImages(ctx, selection.Namespace)
		if err != nil {
			return nil, localImageError(method, selection, err)
		}
		return result, nil
	}
	if !runtime.ValidLocalImageRef(selection.Ref) || !runtime.ValidImageDigest(selection.Identity) {
		return nil, localImageError(method, selection, runtime.ErrLocalImageChanged)
	}
	if method == proto.RPCImageRuntimeImport {
		// This RPC confirms presence immediately. Only the tracked worker may copy layers.
		if selection.Namespace != selection.TargetNamespace {
			return nil, localImageError(method, selection, runtime.ErrLocalImageUnsupported)
		}
		if !i.slot.TryLock() {
			return nil, &proto.RPCError{Code: proto.ErrorLocalImageBusy, Message: "another image operation is in progress; retry when it finishes"}
		}
		defer i.slot.Unlock()
	}
	image, err := d.ResolveLocalImage(ctx, selection, nil)
	if err != nil {
		return nil, localImageError(method, selection, err)
	}
	meta := imagearchive.MetadataFromConfig(image.Architecture, image.Labels)
	copyRequired := false
	if method == proto.RPCImageRuntimePrepare {
		target, ok := d.(runtime.LocalImageCopyTarget)
		if !ok {
			return nil, localImageError(method, selection, runtime.ErrLocalImageUnsupported)
		}
		copyRequired, err = target.NeedsLocalImageCopy(ctx, selection.Ref, image.ID, i.deps.stateDir())
		if err != nil {
			return nil, localImageError(method, selection, err)
		}
	}
	return proto.RuntimeImageDetails{CopyRequired: copyRequired, ImageID: image.ID, RuntimeSizeBytes: image.RuntimeSizeBytes, Metadata: proto.ArchiveImageMetadata(meta)}, nil
}

func localImageError(method string, selection proto.RuntimeImageSelection, err error) *proto.RPCError {
	switch {
	case errors.Is(err, runtime.ErrLocalImageBusy):
		return &proto.RPCError{Code: proto.ErrorLocalImageBusy, Message: runtime.ErrLocalImageBusy.Error()}
	case errors.Is(err, runtime.ErrLocalImageChanged), errors.Is(err, runtime.ErrImageNotFound):
		return &proto.RPCError{Code: proto.ErrorLocalImageChanged, Message: "the selected image changed or disappeared; refresh the list"}
	case errors.Is(err, runtime.ErrLocalImageConflict):
		return &proto.RPCError{Code: proto.ErrorLocalImageConflict, Message: runtime.ErrLocalImageConflict.Error()}
	case errors.Is(err, runtime.ErrLocalImageUnsupported):
		return &proto.RPCError{Code: proto.ErrorLocalImageUnsupported, Message: runtime.ErrLocalImageUnsupported.Error()}
	default:
		cause := err.Error()
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			cause += ": " + string(exit.Stderr)
		}
		cause = strings.TrimSpace(commandlog.Redact(cause))
		const maxDiagnostic = 1024
		if len(cause) > maxDiagnostic {
			cause = cause[:maxDiagnostic] + "…"
		}
		slog.Warn("node local image operation failed", "method", method, "namespace", selection.Namespace,
			"target_namespace", selection.TargetNamespace, "ref", commandlog.Redact(selection.Ref), "reason", cause)
		return &proto.RPCError{Code: proto.ErrorLocalImageUnavailable, Message: "the node could not complete the local image operation"}
	}
}
