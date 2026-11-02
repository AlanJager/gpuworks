package inferenceapi

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// ResolveDeploymentContract resolves the operation contract from the model's
// current editable configuration, so operation changes take effect
// immediately for proxy, Playground, and deployment views. It reads only the
// deployment's model: the Playground calls it once per request, which must
// not read the deployment's instances.
func ResolveDeploymentContract(st *store.Store, deployment *store.Deployment) Contract {
	if st == nil || deployment == nil || deployment.ModelID == 0 {
		return emptyModelContract()
	}
	model, err := st.GetModel(deployment.ModelID)
	if err != nil {
		return emptyModelContract()
	}
	return ModelContract(model)
}

// ModelContract parses and normalizes a model's current editable operations.
// Empty or malformed persisted values deliberately produce no operations;
// callers must not silently infer Chat from legacy capability metadata.
func ModelContract(model *store.Model) Contract {
	if model == nil || strings.TrimSpace(model.Operations) == "" {
		return emptyModelContract()
	}
	var operations []Operation
	if json.Unmarshal([]byte(model.Operations), &operations) != nil {
		return emptyModelContract()
	}
	if len(operations) == 0 {
		return emptyModelContract()
	}
	contract, err := NormalizeContract(Contract{Operations: operations})
	if err != nil {
		return emptyModelContract()
	}
	return contract
}

func emptyModelContract() Contract {
	return Contract{HealthEndpoint: "/health", Operations: []Operation{}}
}

// contractFromJSON decodes and validates a persisted API contract. It remains
// a small compatibility helper for callers and tests that read legacy JSON
// snapshots; model-level deployment resolution itself uses ModelContract.
func contractFromJSON(raw json.RawMessage) (Contract, bool) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return Contract{}, false
	}
	var contract Contract
	if err := json.Unmarshal(raw, &contract); err != nil {
		return Contract{}, false
	}
	normalized, err := NormalizeContract(contract)
	if err != nil {
		return Contract{}, false
	}
	return normalized, true
}
