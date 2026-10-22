package inferencecatalog

import (
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// CatalogWarnings lists what a catalog that already passed validation still
// leaves its author to check, in model and spec order. A warning never blocks
// an import; the template evaluate endpoint and the import response report
// the same list, and this is the only place that produces it (see
// docs/reference/inference-catalog-spec.md). Every warning is a ProductError
// carrying the stable code, an English fallback, and the model_id and spec
// name it is about.
func CatalogWarnings(catalog FlatCatalog) []*messages.ProductError {
	var warnings []*messages.ProductError
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			warnings = append(warnings, specWarnings(model.ModelID, spec)...)
		}
	}
	return warnings
}

func specWarnings(modelID string, spec InferenceSpec) []*messages.ProductError {
	if spec.Engine != engines.Callable {
		return nil
	}
	params := func() map[string]any { return map[string]any{"model_id": modelID, "spec": spec.Name} }
	var warnings []*messages.ProductError
	if spec.API == nil || spec.API.Metrics == nil {
		warnings = append(warnings, messages.NewProductError(messages.WarningCatalogCallableMetricsUndeclared,
			"Callable spec "+spec.Name+" of "+modelID+" declares no api.metrics, so the monitoring center shows no engine metrics for it unless its /metrics happens to use vLLM or SGLang metric names.",
			params()))
	}
	if spec.API == nil || spec.API.HealthEndpoint == "" {
		warnings = append(warnings, messages.NewProductError(messages.WarningCatalogHealthEndpointUndeclared,
			"Callable spec "+spec.Name+" of "+modelID+" declares no api.health_endpoint, so its readiness is probed on the default /health.",
			params()))
	}
	return warnings
}
