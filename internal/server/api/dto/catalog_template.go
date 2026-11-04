package dto

import "gpuworks.ai/gpuworks/internal/server/messages"

// CatalogTemplateExportRequest is POST /api/v1/deployments/{id}/catalog-template.
// IncludeEnv names the deployment's own env keys to keep in the spec; every
// other one is left out, because the export is a file meant to be shared.
type CatalogTemplateExportRequest struct {
	IncludeEnv []string `json:"include_env,omitempty"`
}

// CatalogTemplateExport is a running deployment written as a flat catalog
// template. YAML imports through POST /inference-templates/import as it is;
// its comments state what Notes state: each note is a stable code and the
// values a client interpolates into its own text. Saveable is false when a tested
// operation failed, and then the document is for inspection only.
type CatalogTemplateExport struct {
	ModelID     string                 `json:"model_id"`
	SpecName    string                 `json:"spec_name"`
	YAML        string                 `json:"yaml"`
	Verified    bool                   `json:"verified"`
	Saveable    bool                   `json:"saveable"`
	Checks      []CatalogTemplateCheck `json:"checks"`
	Notes       []messages.Warning     `json:"notes"`
	ExcludedEnv []string               `json:"excluded_env"`
	Evidence    string                 `json:"evidence"`
}

// CatalogTemplateCheck is one operation the export sent a request for
// (tested) or lists without one (a slow or costly operation).
type CatalogTemplateCheck struct {
	Operation  string `json:"operation"`
	Endpoint   string `json:"endpoint,omitempty"`
	Tested     bool   `json:"tested"`
	Passed     bool   `json:"passed"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Message    string `json:"message,omitempty"`
}
