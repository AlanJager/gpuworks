package planning

import (
	"encoding/json"
	"path"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// explainDiskRefusal adds the acquisition context to an existing decision. It
// does not probe a filesystem or change admission/filter precedence.
func explainDiskRefusal(st *store.Store, in Input, req scheduler.Request, snap scheduler.NodeSnapshot, result *scheduler.Result) {
	if result.Message.Code != scheduler.ReasonInsufficientDisk && result.Message.Code != scheduler.ReasonInsufficientRuntimeDisk {
		return
	}
	params := map[string]any{}
	if result.Message.Code == scheduler.ReasonInsufficientRuntimeDisk {
		params["required_bytes"], params["available_bytes"] = req.RuntimeDiskNeedBytes, snap.RuntimeDiskAvailBytes
	} else if req.DiskNeedBytes > 0 && snap.DiskAvailBytes > 0 {
		params["required_bytes"], params["available_bytes"] = req.DiskNeedBytes, snap.DiskAvailBytes
	}
	for key, value := range result.Message.Params {
		params[key] = value
	}
	if in.Node != nil {
		params["node"], params["node_uuid"] = in.Node.DisplayName(), in.Node.UUID
	}
	if result.Message.Code == scheduler.ReasonInsufficientRuntimeDisk {
		if in.Node != nil {
			kind := strings.ToLower(strings.TrimSpace(in.Node.RuntimeDriver))
			if target := observedPath(in.Node, kind, 0); target != "" {
				params["path"] = target
			}
		}
	} else if _, hasStorage := result.Message.Params["storage_id"]; !hasStorage && st != nil && in.Node != nil && in.Model != nil && in.Model.ID != 0 {
		if src, err := st.ModelSourceOn(in.Model.ID, in.Node, in.ModelRules); err == nil && src.Action == store.SourceAcquire {
			primaryBytes := in.Model.WeightBytes
			if primaryBytes <= 0 {
				primaryBytes = in.SpecWeightBytes
			}
			// An aggregate that also budgets unidentified components cannot be
			// attributed to this model's directory or registration action.
			if modelDiskNeed(st, in.Model, in.Node, src.Location.Storage.ID, primaryBytes) == req.DiskNeedBytes {
				params["model"], params["model_id"] = in.Model.DisplayName(), in.Model.ID
				params["storage"], params["storage_id"], params["storage_type"] = src.Location.Storage.Name, src.Location.Storage.ID, src.Location.Storage.Type
				if target := acquisitionTarget(st, in.Node, in.Model, src); target != "" {
					params["path"] = target
				}
			}
		}
	}
	result.Message.Params = params
	for index := range result.Explain.Filters {
		if result.Explain.Filters[index].Code == result.Message.Code {
			result.Explain.Filters[index].Params = params
		}
	}
}

func acquisitionTarget(st *store.Store, node *store.Node, model *store.Model, src store.ModelSource) string {
	rel := store.ModelArtifactPath(model)
	if file := src.Location.File; file != nil && strings.TrimSpace(file.Path) != "" {
		rel = strings.TrimSpace(file.Path)
		if path.IsAbs(rel) {
			return path.Clean(rel)
		}
	}
	eff, applies, err := st.EffectiveStorageForNode(&src.Location.Storage, node)
	if err != nil || !applies {
		return ""
	}
	root := strings.TrimSpace(eff.Path)
	if root == "" {
		root = observedPath(node, "models", 0)
	}
	if !path.IsAbs(root) {
		return ""
	}
	target, ok := store.SharedHostPath(root, rel)
	if !ok {
		return ""
	}
	return target
}

// observedPath returns only an agent-reported managed path. Older model/runtime
// snapshots need not have a total, while storage-specific reports do.
func observedPath(node *store.Node, kind string, storageID uint) string {
	if node == nil {
		return ""
	}
	var paths []proto.PathUsage
	if err := json.Unmarshal([]byte(node.PathUsage), &paths); err != nil {
		return ""
	}
	for _, usage := range paths {
		if usage.Kind == kind && usage.StorageID == storageID && usage.AvailableBytes > 0 {
			// Match the same first usable snapshot capacity selection uses;
			// a later record's directory may belong to a different disk.
			if !path.IsAbs(usage.Path) {
				return ""
			}
			return path.Clean(usage.Path)
		}
	}
	return ""
}
