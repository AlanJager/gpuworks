package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

// Registering weights that already sit on a node: browsing its filesystem,
// reading a directory as a model, and recording the path as a model the
// platform can deploy without downloading anything.

type localFileResponse struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	SizeBytes int64  `json:"size_bytes"`
	Truncated bool   `json:"truncated"`
	// Missing reports an absent file read with missingOK; an agent that
	// predates the flag answers with an error instead.
	Missing bool `json:"missing"`
}

func callNodeFSReadFile(ctx context.Context, call func(context.Context, string, string, any) (json.RawMessage, error), nodeUUID, path string, maxBytes int64, missingOK bool) (localFileResponse, error) {
	params := map[string]any{"path": path, "max_bytes": maxBytes}
	if missingOK {
		params["missing_ok"] = true
	}
	raw, err := call(ctx, nodeUUID, proto.RPCFSReadFile, params)
	if err != nil {
		return localFileResponse{}, err
	}
	var out localFileResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return localFileResponse{}, err
	}
	return out, nil
}

type localModelPreview struct {
	NodeUUID        string                   `json:"node_uuid"`
	Path            string                   `json:"path"`
	SourceID        string                   `json:"source_id"`
	Name            string                   `json:"name"`
	Arch            string                   `json:"arch,omitempty"`
	Precision       string                   `json:"precision,omitempty"`
	ContextLength   int64                    `json:"context_length,omitempty"`
	WeightBytes     int64                    `json:"weight_bytes"`
	Operations      []inferenceapi.Operation `json:"operations"`
	InputModalities []string                 `json:"input_modalities,omitempty"`
	ConfigRaw       string                   `json:"config_raw,omitempty"`
	README          string                   `json:"readme,omitempty"`
	READMETruncated bool                     `json:"readme_truncated,omitempty"`
}

func (a *apiHandlers) previewLocalModel(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	var req struct {
		NodeUUID string `json:"node_uuid"`
		Path     string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	preview, err := a.buildLocalModelPreview(r.Context(), req.NodeUUID, req.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (a *apiHandlers) buildLocalModelPreview(ctx context.Context, nodeUUID, path string) (localModelPreview, error) {
	path = strings.TrimSpace(path)
	if nodeUUID == "" || path == "" {
		return localModelPreview{}, productError(ErrorLocalModelFieldsRequired, "node_uuid and path are required", nil)
	}
	if _, err := a.d.Store.NodeByUUID(nodeUUID); err != nil {
		return localModelPreview{}, err
	}
	reg, err := callNodeParseModelMetadata(ctx, a.d.CallNode, nodeUUID, path, 0)
	if err != nil {
		return localModelPreview{}, fmt.Errorf("validate local model: %w", err)
	}
	if err := validateModelMetadata(reg.Metadata); err != nil {
		return localModelPreview{}, err
	}
	dir, err := a.modelDirFromMetadata(ctx, nodeUUID, path, reg.Metadata)
	if err != nil {
		return localModelPreview{}, err
	}
	sourceID := filepath.Base(filepath.Clean(path))
	if sourceID == "." || sourceID == string(filepath.Separator) || sourceID == "" {
		sourceID = path
	}
	return localModelPreview{NodeUUID: nodeUUID, Path: reg.Path, SourceID: sourceID, Name: sourceID, Arch: archOfConfig(dir.config), Precision: modelmeta.Precision(dir.config, dir.configRaw), ContextLength: modelmeta.ContextLength(dir.config, dir.configRaw), WeightBytes: reg.SizeBytes, Operations: inferenceapi.OperationsForTasks(dir.tasks), InputModalities: inferredModelModalities(dir.config), ConfigRaw: dir.configRaw, README: dir.readme, READMETruncated: dir.readmeTruncated}, nil
}

// modelDir is what a model directory on a node says about the model: its
// config.json, parsed and raw, and its README. A directory without config.json
// is still a model - what it must contain is the engine's business, not the
// platform's (a GGUF-only repository has none) - it just states no metadata,
// so config is nil and configRaw empty.
type modelDir struct {
	config          *modelconfig.Config
	configRaw       string
	readme          string
	readmeTruncated bool
	tasks           []string
}

// modelDirFromMetadata consumes agent-parsed metadata. The fallback reads
// text metadata from an older agent that does not yet return a report.
func (a *apiHandlers) modelDirFromMetadata(ctx context.Context, nodeUUID, dir string, report *modelmetadata.Report) (modelDir, error) {
	if report != nil {
		out := modelDir{configRaw: report.ConfigRaw, readme: report.README, tasks: report.Tasks}
		if report.ConfigRaw != "" {
			out.config, _ = modelconfig.Parse(strings.NewReader(report.ConfigRaw))
		}
		return out, nil
	}
	// Older agents return no metadata report. Keep their text metadata path
	// during a rolling upgrade, using the same task parser.

	dir = strings.TrimRight(dir, "/")
	config, err := callNodeFSReadFile(ctx, a.d.CallNode, nodeUUID, dir+"/config.json", 512*1024, true)
	if err != nil {
		return modelDir{}, fmt.Errorf("read config.json: %w", err)
	}
	out := modelDir{}
	if !config.Missing {
		cfg, err := modelconfig.Parse(strings.NewReader(config.Content))
		if err != nil {
			return modelDir{}, fmt.Errorf("parse config.json: %w", err)
		}
		out.config, out.configRaw = cfg, config.Content
	}
	readme, _ := callNodeFSReadFile(ctx, a.d.CallNode, nodeUUID, dir+"/README.md", 512*1024, false)
	out.readme, out.readmeTruncated = readme.Content, readme.Truncated
	out.tasks = modelmetadata.InferTasks(modelmetadata.Documents{Config: out.configRaw, README: out.readme}).Tasks
	return out, nil
}

// model is the model row a directory describes, under the identity given.
func (d modelDir) model(source, sourceID, revision, name string, weightBytes int64) *store.Model {
	operations, _ := json.Marshal(inferenceapi.OperationsForTasks(d.tasks))
	modalities, _ := json.Marshal(inferredModelModalities(d.config))
	precision := modelmeta.Precision(d.config, d.configRaw)
	return &store.Model{
		Source: source, SourceID: sourceID, Revision: revision, Name: name, Arch: archOfConfig(d.config),
		WeightBytes: weightBytes, Dtype: precision, Meta: d.configRaw, Operations: string(operations),
		InputModalities: string(modalities), README: d.readme, ContextLength: modelmeta.ContextLength(d.config, d.configRaw),
		Precision: precision,
	}
}

func archOfConfig(cfg *modelconfig.Config) string {
	if cfg == nil {
		return ""
	}
	if len(cfg.Architectures) > 0 {
		return cfg.Architectures[0]
	}
	return cfg.ModelType
}

func (a *apiHandlers) registerLocalModel(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	var req struct {
		NodeUUID  string `json:"node_uuid"`
		Path      string `json:"path"`
		SourceID  string `json:"source_id"`
		Name      string `json:"name"`
		StorageID uint   `json:"storage_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validateName(w, req.Name) {
		return
	}
	preview, err := a.buildLocalModelPreview(r.Context(), req.NodeUUID, req.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.SourceID) != "" {
		preview.SourceID = strings.TrimSpace(req.SourceID)
	}
	if strings.TrimSpace(req.Name) != "" {
		preview.Name = strings.TrimSpace(req.Name)
	}
	if !validateName(w, preview.Name) {
		return
	}
	operationsJSON, _ := json.Marshal(preview.Operations)
	modalitiesJSON, _ := json.Marshal(preview.InputModalities)
	// The preview is what the person confirmed, so the row is built from it
	// rather than read again.
	entity := &store.Model{Source: "local", SourceID: preview.SourceID, Revision: "local", Name: preview.Name, Arch: preview.Arch, WeightBytes: preview.WeightBytes, Dtype: preview.Precision, Meta: preview.ConfigRaw, Operations: string(operationsJSON), InputModalities: string(modalitiesJSON), README: preview.README, ContextLength: preview.ContextLength, Precision: preview.Precision}
	node, err := a.d.Store.NodeByUUID(req.NodeUUID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	storage, err := a.localStorageForNode(0, node, req.StorageID)
	if err != nil || storage.Type != proto.StorageLocal {
		writeProductError(w, http.StatusBadRequest, ErrorStorageTypeInvalid, "a node's directory is registered into a local storage", nil)
		return
	}
	m, _, err := a.d.Store.RegisterLocalModel(entity, store.FileRef{StorageID: storage.ID, NodeID: node.ID}, preview.Path, preview.WeightBytes)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.modelDTO(m, true))
}

func (a *apiHandlers) registerModelPath(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	var req struct {
		NodeUUID  string `json:"node_uuid"`
		Path      string `json:"path"`
		StorageID uint   `json:"storage_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		writeProductError(w, http.StatusBadRequest, ErrorModelPathRequired, "path is required", nil)
		return
	}
	m, err := a.d.Store.GetModel(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	node, err := a.d.Store.NodeByUUID(req.NodeUUID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// A model registered while the management node could not read its
	// source has no metadata yet: the directory being registered is asked
	// for it in the same call.
	reg, err := callNodeRegisterPath(r.Context(), a.d.CallNode, node.UUID, req.Path, lacksMetadata(m))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	storage, err := a.localStorageForNode(m.ID, node, req.StorageID)
	if err != nil || storage.Type != proto.StorageLocal {
		writeProductError(w, http.StatusBadRequest, ErrorStorageTypeInvalid, "a node's directory is registered into a local storage", nil)
		return
	}
	if _, err := a.d.Store.RegisterModelFile(store.FileRef{ModelID: m.ID, StorageID: storage.ID, NodeID: node.ID}, reg.Path, reg.SizeBytes); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.completeFromLocalCopy(r.Context(), m, node, reg); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}

// fsListResponse is the agent's fs.list answer (proto.RPCFSList).
type fsListResponse struct {
	Path    string    `json:"path"`
	Entries []fsEntry `json:"entries"`
}

// fsListing is GET /nodes/{uuid}/fs: one directory and its entries.
type fsListing struct {
	Path  string    `json:"path"`
	Items []fsEntry `json:"items"`
	Total int       `json:"total"`
}

type fsEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type registerPathResponse struct {
	Path      string                `json:"path"`
	SizeBytes int64                 `json:"size_bytes"`
	Metadata  *modelmetadata.Report `json:"metadata,omitempty"`
	// Revision is the commit a huggingface download left in the directory,
	// when all its files share one; agents that predate it send none.
	Revision string `json:"revision,omitempty"`
}

func callNodeFSList(ctx context.Context, call func(context.Context, string, string, any) (json.RawMessage, error), nodeUUID, path string) (fsListResponse, error) {
	raw, err := call(ctx, nodeUUID, proto.RPCFSList, map[string]string{"path": path})
	if err != nil {
		return fsListResponse{}, err
	}
	var out fsListResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return fsListResponse{}, err
	}
	return out, nil
}

func callNodeRegisterPath(ctx context.Context, call func(context.Context, string, string, any) (json.RawMessage, error), nodeUUID, path string, inspectMetadata bool) (registerPathResponse, error) {
	raw, err := call(ctx, nodeUUID, proto.RPCRegisterPath, map[string]any{"path": path, "inspect_metadata": inspectMetadata})
	if err != nil {
		return registerPathResponse{}, err
	}
	var out registerPathResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return registerPathResponse{}, err
	}
	if strings.TrimSpace(out.Path) == "" {
		out.Path = path
	}
	return out, nil
}
