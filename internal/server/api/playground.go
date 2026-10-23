package api

import (
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) playgroundModels(w http.ResponseWriter, r *http.Request) {
	parameters, extraParameterPresets := playgroundDictionary()
	catalog := dto.PlaygroundCatalog{
		Models:                []dto.PlaygroundModel{},
		Parameters:            parameters,
		ExtraParameterPresets: extraParameterPresets,
		StreamOptions:         playgroundStreamOptions(),
		HTMLPreview:           dto.PlaygroundHTMLPreview{AllowNetwork: a.d.Store.SecurityPolicy().PlaygroundPreviewNetwork},
	}

	deps, err := a.d.Store.ListDeployments()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	type deploymentCandidate struct {
		deployment *store.Deployment
		instances  []store.Instance
		score      int
	}
	candidates := make([]deploymentCandidate, 0, len(deps))
	byServedName := make(map[string]int, len(deps))
	for i := range deps {
		dep := &deps[i]
		if dep.State != store.DeployActive || strings.TrimSpace(dep.ServedName) == "" {
			continue
		}
		instances, _ := a.d.Store.InstancesForDeployment(dep.ID)
		candidate := deploymentCandidate{deployment: dep, instances: instances, score: playgroundDeploymentScore(instances)}
		if index, exists := byServedName[dep.ServedName]; exists {
			// ListDeployments is newest-first. Keep that deterministic fallback,
			// but never let a replacement that is still starting hide the
			// rollout which is already serving (or sleeping and can be woken).
			if candidate.score > candidates[index].score {
				candidates[index] = candidate
			}
			continue
		}
		byServedName[dep.ServedName] = len(candidates)
		candidates = append(candidates, candidate)
	}
	for _, candidate := range candidates {
		dep := candidate.deployment
		model, err := a.d.Store.GetModel(dep.ModelID)
		if err != nil {
			continue
		}
		modelView := a.modelDTO(model, false)
		sleeping := false
		for _, instance := range candidate.instances {
			if instance.SleepLevel > 0 {
				sleeping = true
				break
			}
		}
		contract := inferenceapi.ResolveDeploymentContract(a.d.Store, dep, candidate.instances)
		if len(contract.Operations) == 0 {
			// An explicitly empty or invalid model operation configuration has no
			// callable Playground entry. Do not synthesize Chat as a fallback.
			continue
		}
		hasChat := false
		for _, operation := range contract.Operations {
			if operation.Type == inferenceapi.OperationChat {
				hasChat = true
				break
			}
		}
		catalog.Models = append(catalog.Models, dto.PlaygroundModel{
			ID: dep.ServedName, Label: dep.ServedName, Operations: playgroundOperations(contract, dep.ServedName),
			InputModalities: modelView.InputModalities, ContextLength: model.ContextLength,
			Sleeping: sleeping, DeploymentID: dep.ID, TemplateID: dep.TemplateID, TemplateSpecSource: dep.TemplateSpecSource, TemplateSpecName: dep.TemplateSpecName, PublicDataPlane: true,
			Engine: dep.Engine, ThinkingToggle: playgroundThinkingToggleEnabled(dep.Engine, dep.ServedName, model, hasChat),
		})
	}

	writeJSON(w, http.StatusOK, catalog)
}

func playgroundDeploymentScore(instances []store.Instance) int {
	score := 0
	for _, instance := range instances {
		if instance.State != proto.InstRunning || instance.Headless || instance.APIPort <= 0 {
			continue
		}
		if instance.SleepLevel == 0 {
			return 2
		}
		score = 1
	}
	return score
}

func playgroundOperations(contract inferenceapi.Contract, model string) []dto.PlaygroundOperation {
	result := make([]dto.PlaygroundOperation, 0, len(contract.Operations))
	for _, operation := range contract.Operations {
		definition, ok := inferenceapi.Lookup(operation.Type)
		if !ok {
			continue
		}
		operationView := dto.PlaygroundOperation{
			RequestDefaults: operation.RequestDefaults, Inputs: append([]inferenceapi.InputField{}, operation.Inputs...),
			PrimaryInputs: definition.PrimaryInputs, Discovery: definition.Discovery,
			Type: operation.Type, Endpoint: operation.Endpoint, Mode: operation.Mode,
			RequestEncoding:  operation.RequestEncoding,
			RequestEncodings: definition.RequestEncodings, Renderer: definition.Renderer,
			Response: definition.Response, Async: definition.Async,
			ExampleInput: operation.ExampleInput,
		}
		if example, err := inferenceapi.BuildExample(operation, model); err == nil {
			operationView.Example = &example
		}
		result = append(result, operationView)
	}
	return result
}
