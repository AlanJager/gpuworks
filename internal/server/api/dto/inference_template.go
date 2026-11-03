package dto

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// InferenceTemplatePage is one page of GET /api/v1/inference-templates.
type InferenceTemplatePage = ListPage[InferenceTemplate]

// InferenceDetectedSupport is a read-only projection of catalog rules onto
// detected hardware. Raw unsupported rules remain separately available.
type InferenceDetectedSupport struct {
	PreferredEngine string                     `json:"preferred_engine"`
	Warnings        []InferenceDetectedWarning `json:"warnings"`
}
type InferenceDetectedWarning struct {
	Reason               string `json:"reason"`
	Quantization         string `json:"quantization,omitempty"`
	AlternativeAvailable bool   `json:"alternative_available"`
}

type InferenceTemplate struct {
	DetectedSupport   *InferenceDetectedSupport `json:"detected_support,omitempty"`
	ID                uint                      `json:"id"`
	ModelID           string                    `json:"model_id"`
	DisplayTitle      string                    `json:"display_title,omitempty"`
	ParameterCount    string                    `json:"parameter_count,omitempty"`
	Architecture      string                    `json:"architecture,omitempty"`
	InputModalities   []string                  `json:"input_modalities,omitempty"`
	ReadmeZHCN        string                    `json:"readme_zh_cn,omitempty"`
	ReadmeEN          string                    `json:"readme_en,omitempty"`
	ReadmeZHCNContent string                    `json:"readme_zh_cn_content,omitempty"`
	ReadmeENContent   string                    `json:"readme_en_content,omitempty"`
	ContextLength     int64                     `json:"context_length,omitempty"`
	ReleaseDate       string                    `json:"release_date,omitempty"`
	Source            string                    `json:"source"`
	Unsupported       []InferenceUnsupported    `json:"unsupported,omitempty"`
	Specs             []InferenceSpecDTO        `json:"specs"`
}

type InferenceSpecDTO struct {
	// PDMode is set on a spec deployed only as PD in that mode.
	PDMode             string            `json:"pd_mode,omitempty"`
	ID                 uint              `json:"id"`
	Source             string            `json:"source"`
	Name               string            `json:"name"`
	DisplayNameZHCN    string            `json:"display_name_zh_cn,omitempty"` // human-written name; the client shows the one for its locale
	DisplayNameEN      string            `json:"display_name_en,omitempty"`
	Scenario           string            `json:"scenario,omitempty"`
	Engine             string            `json:"engine"`
	Image              string            `json:"image"`
	Quantization       string            `json:"quantization,omitempty"`
	WeightSources      []WeightSource    `json:"weight_sources"`
	WeightBytes        int64             `json:"weight_bytes,omitempty"`
	RuntimeWeightBytes int64             `json:"runtime_weight_bytes,omitempty"`
	ShmBytes           int64             `json:"shm_bytes,omitempty"`         // the /dev/shm size the engine needs; omitted when the spec states none
	MemlockUnlimited   bool              `json:"memlock_unlimited,omitempty"` // the engine needs unlimited locked memory; omitted when false
	GPUs               GPUPredicate      `json:"gpus"`
	Args               []string          `json:"args,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	PreStartArgv       []string          `json:"pre_start_argv,omitempty"`
	PreStartScript     string            `json:"pre_start_script,omitempty"`
	Assets             []InferenceAsset  `json:"assets,omitempty"`
	Entrypoint         []string          `json:"entrypoint,omitempty"`
	// EngineDecidesParamKeys are the dictionary keys whose default this spec's
	// launch leaves to the engine (deploy.EngineDecidesParameterKeys): the form
	// starts them at automatic. Derived by the engine adapter; catalog authors
	// do not maintain a parallel capability list.
	EngineDecidesParamKeys []string         `json:"engine_decides_param_keys,omitempty"`
	Placement              *Placement       `json:"placement,omitempty"`
	API                    *APIContract     `json:"api,omitempty"`
	Components             []ModelComponent `json:"components,omitempty"`

	// ImageSources are further references the template offers for pulling Image,
	// preferred first; Image itself is tried after them (ADR 0038).
	ImageSources []string `json:"image_sources,omitempty"`
}

type InferenceAsset struct {
	File      string `json:"file"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	UnpackTo  string `json:"unpack_to,omitempty"`
}

type Placement struct {
	Language string `json:"language"`
	Resolver string `json:"resolver"`
}

type APIContract struct {
	HealthEndpoint string         `json:"health_endpoint,omitempty"`
	Operations     []APIOperation `json:"operations"`
	Metrics        *APIMetrics    `json:"metrics,omitempty"`
	// RequestStats is api.request_stats: reply field path to request stat
	// name, or {"to": name, "scale": factor} when it scales.
	RequestStats inferenceapi.MetricMap `json:"request_stats,omitempty"`
}

// APIMetrics is a spec's api.metrics: the engine's Prometheus endpoint and how
// its families map onto platform inference metrics. A map value is the
// metric's short name, or {"to": name, "scale": factor} when it is scaled;
// the editor writes it back as it reads it.
type APIMetrics struct {
	Endpoint string                     `json:"endpoint,omitempty"`
	Format   string                     `json:"format,omitempty"`
	Inherit  string                     `json:"inherit,omitempty"`
	Map      map[string]json.RawMessage `json:"map,omitempty"`
}

type APIOperation struct {
	Type             string                       `json:"type"`
	Endpoint         string                       `json:"endpoint"`
	Mode             string                       `json:"mode"`
	RequestEncodings []string                     `json:"request_encodings"`
	Renderer         string                       `json:"renderer"`
	Response         string                       `json:"response"`
	Async            *inferenceapi.AsyncLifecycle `json:"async,omitempty"`
	Example          *inferenceapi.Example        `json:"example,omitempty"`
}

type ModelComponent struct {
	Name        string          `json:"name"`
	Role        string          `json:"role"`
	ModelID     string          `json:"model_id,omitempty"`
	Weights     ComponentWeight `json:"weights"`
	WeightBytes int64           `json:"weight_bytes"`
	MountPath   string          `json:"mount_path"`
}

type ComponentWeight struct {
	Quantization string         `json:"quantization,omitempty"`
	Sources      []WeightSource `json:"sources"`
}

type WeightSource struct {
	Provider  string `json:"provider"`
	RepoID    string `json:"repo_id"`
	Revision  string `json:"revision,omitempty"`
	Subfolder string `json:"subfolder,omitempty"`
}

type GPUPredicate struct {
	Vendor string `json:"vendor"`
	CC     string `json:"cc,omitempty"`
	Name   string `json:"name,omitempty"`
	ArchID string `json:"arch_id,omitempty"`
}

type InferenceUnsupported struct {
	GPUs         GPUPredicate `json:"gpus"`
	Quantization string       `json:"quantization,omitempty"`
	Reason       string       `json:"reason"`
}

type InferenceTemplateIndex struct {
	Hardware []DetectedTemplateChip       `json:"hardware,omitempty"`
	Items    []InferenceTemplateIndexItem `json:"items"`
}

type InferenceTemplateIndexItem struct {
	ID           uint                      `json:"id"`
	ModelID      string                    `json:"model_id"`
	DisplayTitle string                    `json:"display_title,omitempty"`
	Source       string                    `json:"source"`
	Engines      []string                  `json:"engines"`
	Scenarios    []string                  `json:"scenarios"`
	Profiles     []InferenceProfileSummary `json:"profiles"`
}

// InferenceProfileSummary is the compact, hardware-filterable representation
// used by the deployment wizard before a full template detail is selected.
type InferenceProfileSummary struct {
	DetectedCompatible *bool        `json:"detected_compatible,omitempty"`
	Source             string       `json:"source"`
	Name               string       `json:"name"`
	DisplayNameZHCN    string       `json:"display_name_zh_cn,omitempty"`
	DisplayNameEN      string       `json:"display_name_en,omitempty"`
	Scenario           string       `json:"scenario,omitempty"`
	Engine             string       `json:"engine"`
	Image              string       `json:"image,omitempty"`
	Quantization       string       `json:"quantization,omitempty"`
	WeightBytes        int64        `json:"weight_bytes,omitempty"`
	WeightRepoIDs      []string     `json:"weight_repo_ids,omitempty"`
	GPUs               GPUPredicate `json:"gpus"`
}

type InferenceTemplateOrganizations struct {
	Items []InferenceTemplateOrganization `json:"items"`
}

type InferenceTemplateOrganization struct {
	Organization string `json:"organization"`
	Models       int64  `json:"models"`
}

type InferenceTemplateSummary struct {
	Deployable int64 `json:"deployable"`
	Models     int64 `json:"models"`
}

type TemplateChip struct {
	Vendor       string  `json:"vendor"`
	Name         string  `json:"name"`
	CC           float64 `json:"cc,omitempty"`
	Category     string  `json:"category"`
	Architecture string  `json:"architecture"`
	ArchID       string  `json:"arch_id,omitempty"`
}

type DetectedTemplateChip struct {
	Vendor string `json:"vendor"`
	Name   string `json:"name"`
	CC     string `json:"cc,omitempty"`
	ArchID string `json:"arch_id,omitempty"`
}

// InferenceTemplateEvaluation is the answer of POST /inference-templates/evaluate:
// what an import of the same body would find, without writing anything. Valid
// is true exactly when Errors is empty.
type InferenceTemplateEvaluation struct {
	Valid    bool             `json:"valid"`
	Errors   []messages.Issue `json:"errors"`
	Warnings []messages.Issue `json:"warnings"`
}

// InferenceTemplateImportResult is the answer of POST /inference-templates/import.
// Warnings is the same list evaluate returns for the imported models and is
// left out when empty.
type InferenceTemplateImportResult struct {
	Models        int              `json:"models"`
	PackageDigest string           `json:"package_digest"`
	Warnings      []messages.Issue `json:"warnings,omitempty"`
}
