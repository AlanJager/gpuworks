package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// urlModelPreview is what the register form shows after a directory URL is
// parsed and before anything is written. It mirrors the local-path preview:
// the operator sees the identity that will be stored plus enough of the
// model's own facts to confirm the URL points at the intended weights.
type urlModelPreview struct {
	SourceURL       string   `json:"source_url"`
	SourceID        string   `json:"source_id"`
	Name            string   `json:"name"`
	Arch            string   `json:"arch,omitempty"`
	Precision       string   `json:"precision,omitempty"`
	ContextLength   int64    `json:"context_length,omitempty"`
	WeightBytes     int64    `json:"weight_bytes"`
	FilesCount      int      `json:"files_count"`
	InputModalities []string `json:"input_modalities,omitempty"`
}

func (a *apiHandlers) previewURLModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceURLRequired, "url is required", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	client := outbound.MetadataClient(&http.Client{Transport: a.outboundTransport()})
	defer client.CloseIdleConnections()
	preview, err := modelmeta.PreviewDirectoryWithClient(ctx, req.URL, client)
	if err != nil {
		writeDirectoryError(w, req.URL, err)
		return
	}
	writeJSON(w, http.StatusOK, urlPreviewDTO(preview))
}

func (a *apiHandlers) registerURLModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceURL string `json:"source_url"`
		SourceID  string `json:"source_id"`
		Name      string `json:"name"`
	}
	if !decodeJSONBody(w, r, &req) || !validateName(w, req.Name) {
		return
	}
	if strings.TrimSpace(req.SourceURL) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceURLRequired, "source_url is required", nil)
		return
	}
	// The directory is read again rather than trusted from the request: the
	// preview the operator confirmed may be minutes old, and registering a
	// directory that no longer holds a model would produce a row nothing can
	// deploy.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	client := outbound.MetadataClient(&http.Client{Transport: a.outboundTransport()})
	defer client.CloseIdleConnections()
	preview, err := modelmeta.PreviewDirectoryWithClient(ctx, req.SourceURL, client)
	if err != nil {
		writeDirectoryError(w, req.SourceURL, err)
		return
	}
	sourceID := strings.TrimSpace(req.SourceID)
	if sourceID == "" {
		sourceID = preview.SourceID
	}
	if sourceID == "" {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceIDRequired, "source_id is required", nil)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = preview.Name
	}
	if name == "" {
		name = sourceID
	}
	if !validateName(w, name) {
		return
	}
	// A directory index has no commit history, so the revision stays empty and
	// the artifact path keeps the unpinned identity. Everything else comes from
	// the same metadata path a Hugging Face registration uses, with the
	// directory itself as the endpoint.
	entity := &store.Model{
		Source: modelmeta.SourceURL, SourceID: sourceID, SourceURL: preview.SourceURL,
		Revision: "", Name: name,
	}
	var notice *dto.ProductMessage
	if meta, err := a.fetchModelMeta(r.Context(), entity.Source, entity.SourceID, entity.Revision, entity.ArtifactSubfolder, entity.SourceURL); err == nil {
		notice = a.metadataNotice(meta, entity.Source, entity.SourceURL)
		a.applyFetchedMetadata(r.Context(), entity, meta)
	}
	initRegisteredModelContract(entity)
	m, err := a.d.Store.UpsertModel(entity)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := a.modelDTO(m, true)
	out.MetadataNotice = notice
	writeJSON(w, http.StatusCreated, out)
}

func urlPreviewDTO(preview *modelmeta.DirectoryPreview) urlModelPreview {
	return urlModelPreview{
		SourceURL: preview.SourceURL, SourceID: preview.SourceID, Name: preview.Name,
		Arch: preview.Arch, Precision: preview.Precision, ContextLength: preview.ContextLength,
		WeightBytes: preview.WeightBytes, FilesCount: preview.FilesCount,
		InputModalities: preview.InputModalities,
	}
}

// writeDirectoryError distinguishes invalid input from directory failures
// using the product errors a client renders, so the operator is told what
// to change rather than being shown a transport message.
func writeDirectoryError(w http.ResponseWriter, raw string, err error) {
	params := map[string]any{"url": strings.TrimSpace(raw), "detail": "the model source could not be read"}
	switch {
	case errors.Is(err, modelmeta.ErrDirectoryURLInvalid):
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceURLInvalid,
			"model directory URL must be an absolute HTTP or HTTPS URL without embedded credentials", nil)
	case errors.Is(err, modelmeta.ErrDirectoryNotAModel):
		writeProductError(w, http.StatusBadRequest, ErrorHTTPDirectoryNotAModel,
			"the directory lists no files, or its config.json does not parse", params)
	case errors.Is(err, modelmeta.ErrDirectoryTooLarge):
		writeProductError(w, http.StatusBadRequest, ErrorHTTPDirectoryTooLarge,
			"the directory has more files or nesting than a model repository can have", params)
	default:
		writeProductError(w, http.StatusBadGateway, ErrorHTTPDirectoryUnreachable,
			"the directory could not be read as an HTTP directory index", params)
	}
}

// modelSourceEndpoint is the repository endpoint a model's source requests go
// to. Only an HTTP directory carries one: the directory URL is the repository,
// and nothing else in the row can reproduce it, so every call that hands a
// model to modelrepo or modelmeta passes it through here rather than assuming
// the source's public host.
func modelSourceEndpoint(model *store.Model) string {
	if model == nil || !strings.EqualFold(strings.TrimSpace(model.Source), modelmeta.SourceURL) {
		return ""
	}
	return strings.TrimSpace(model.SourceURL)
}

// minAgentVersionForURLModels is the first tagged release whose agent can
// download a Source="url" model. An older agent's file listing would treat the
// source as one it does not know and fail on the node with an error about the
// wrong host, so the download is refused here instead, where the message can
// say what to do.
const minAgentVersionForURLModels = "26.11.0"

// agentPredatesURLModels reports whether a node's reported agent version is
// provably older than the release that added HTTP directory downloads.
//
// Only a version that parses as a release is ever judged. A development build
// reports something that is not a release number at all ("dev", a bare commit
// id, an empty string on a node that has not said hello yet), and there is no
// way to order such a build against a release — including the build of this
// very tree, which does carry the feature. Refusing what cannot be ordered
// would make the feature impossible to run outside a tagged release, so an
// unparseable version is allowed through and left to fail, if it is genuinely
// old, with the agent's own listing error.
func agentPredatesURLModels(agentVersion string) bool {
	agent, ok := parseReleaseVersion(agentVersion)
	if !ok {
		return false
	}
	minimum, ok := parseReleaseVersion(minAgentVersionForURLModels)
	if !ok {
		return false
	}
	for index := range agent {
		if agent[index] != minimum[index] {
			return agent[index] < minimum[index]
		}
	}
	return false
}

// parseReleaseVersion reads the major.minor.patch prefix of a release version.
// A build suffix ("1.2.0-14-gabc1234", produced by git describe after a tag)
// describes a build after that tag, so it orders with the tag it names.
func parseReleaseVersion(value string) ([3]int, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	if cut := strings.IndexAny(value, "-+"); cut >= 0 {
		value = value[:cut]
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var parsed [3]int
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return [3]int{}, false
		}
		parsed[index] = number
	}
	return parsed, true
}

// admitModelDownloadNode refuses a download onto a node whose agent cannot
// perform it. Registration is never gated: it is a global act that names no
// node, and an operator must be able to register a model before the nodes that
// will hold it are upgraded.
func admitModelDownloadNode(model *store.Model, node *store.Node) error {
	if model == nil || node == nil || modelSourceEndpoint(model) == "" {
		return nil
	}
	if !agentPredatesURLModels(node.AgentVersion) {
		return nil
	}
	return productError(ErrorModelSourceRequiresNewerAgent,
		"the node's agent is too old to download a model from an HTTP directory; upgrade the agent first",
		map[string]any{"node": node.UUID, "agent_version": node.AgentVersion, "required_version": minAgentVersionForURLModels})
}

// refreshModelMetadataFor re-reads a model's metadata from its own source.
func (a *apiHandlers) refreshModelMetadataFor(ctx context.Context, model *store.Model) (*modelmeta.Meta, error) {
	return a.fetchModelMeta(ctx, model.Source, model.SourceID, model.Revision, model.ArtifactSubfolder, model.SourceURL)
}

// modelSourceHost names a model's source for a person when the provider name
// does not: every HTTP directory reports the provider "url", and only the host
// distinguishes the mirror one deployment downloads from.
func modelSourceHost(model *store.Model) string {
	endpoint := modelSourceEndpoint(model)
	if endpoint == "" {
		return ""
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return parsed.Host
}
