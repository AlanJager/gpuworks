// Package messages defines language-neutral product messages shared by the
// server's domain services and API layer. Human-readable text belongs to the
// client locale resources; the wire contract contains only a stable code and
// interpolation parameters.
package messages

// Warning is a non-blocking, language-neutral product warning.
type Warning struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// Warning codes used by deployment evaluation.
const (
	WarningModelNotFound                   = "MODEL_NOT_FOUND"
	WarningImageOverrideIncompatible       = "IMAGE_OVERRIDE_INCOMPATIBLE"
	WarningImageEngineDefaultIncompatible  = "IMAGE_ENGINE_DEFAULT_INCOMPATIBLE"
	WarningImageOverrideUnverified         = "IMAGE_OVERRIDE_UNVERIFIED"
	WarningImageDigestUnverified           = "IMAGE_DIGEST_UNVERIFIED"
	WarningImageCatalogNoMatch             = "IMAGE_CATALOG_NO_MATCH"
	WarningImageCatalogUnavailable         = "IMAGE_CATALOG_UNAVAILABLE"
	WarningPlanningEngineUnavailable       = "PLANNING_ENGINE_UNAVAILABLE"
	WarningPlanningGPUArchCaution          = "PLANNING_GPU_ARCH_CAUTION"
	WarningPlanningModelWeightUnknown      = "PLANNING_MODEL_WEIGHT_UNKNOWN"
	WarningImagePartitionProfileUnbuilt    = "IMAGE_PARTITION_PROFILE_UNBUILT"
	WarningImagePartitionProfileUnverified = "IMAGE_PARTITION_PROFILE_UNVERIFIED"
	WarningImageCatalogNoMatchDomestic     = "IMAGE_CATALOG_NO_MATCH_DOMESTIC"
	WarningImageCatalogUnavailableDomestic = "IMAGE_CATALOG_UNAVAILABLE_DOMESTIC"
	WarningImageDriverCompatibility        = "IMAGE_DRIVER_COMPATIBILITY"
	// WarningComputeNetworkFallback: some ranks have a compute network of
	// their own, but not all share it, so the group runs over the management
	// network.
	WarningComputeNetworkFallback = "COMPUTE_NETWORK_FALLBACK"
	// WarningPlanningKVBelowContext: the deployment starts, but the KV pool
	// it is estimated to get holds fewer tokens than one request of the
	// maximum context - vLLM refuses to start on exactly that (the estimate is
	// conservative, so near the line it may still start). A dGPU slice
	// sized for the weights with the model's default context is the usual
	// way here.
	WarningPlanningKVBelowContext = "PLANNING_KV_BELOW_CONTEXT"
)

// NewWarning constructs a warning while keeping empty parameter maps off the
// wire. It is intentionally small so domain packages do not need API helpers.
func NewWarning(code string, params map[string]any) Warning {
	if len(params) == 0 {
		params = nil
	}
	return Warning{Code: code, Params: params}
}

// Issue is a product error or warning in wire form where the two travel in
// lists: the embedded Warning is the stable code and the params that
// interpolate into the client's localized text; Error is the English text, the
// key an error response carries it under.
type Issue struct {
	Warning
	Error string `json:"error"`
}
