package inferenceapi

import (
	"fmt"
	"strings"
)

// NormalizeModelConfig validates the model-level editable operation and input
// modality arrays. An empty operation list is valid and intentionally means
// that the model has no Playground entry.
func NormalizeModelConfig(operations []Operation, inputModalities []string) ([]Operation, []string, error) {
	var normalized []Operation
	if len(operations) > 0 {
		contract, err := NormalizeContract(Contract{Operations: operations})
		if err != nil {
			return nil, nil, err
		}
		normalized = contract.Operations
	}
	modalities := make([]string, 0, len(inputModalities))
	seen := map[string]bool{}
	for index, modality := range inputModalities {
		modality = strings.ToLower(strings.TrimSpace(modality))
		if modality != "text" && modality != "image" && modality != "audio" && modality != "video" {
			return nil, nil, fmt.Errorf("input_modalities[%d] has unsupported value %q", index, modality)
		}
		if !seen[modality] {
			seen[modality] = true
			modalities = append(modalities, modality)
		}
	}
	return normalized, modalities, nil
}
