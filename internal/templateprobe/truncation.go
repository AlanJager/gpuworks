package templateprobe

import (
	"fmt"
	"strings"
)

type TruncationKind string

const (
	TruncationDense       TruncationKind = "dense"
	TruncationDeepSeekMoE TruncationKind = "deepseek_moe"
	TruncationQwenMoE     TruncationKind = "qwen3_moe"
	TruncationHybrid      TruncationKind = "hybrid"
	TruncationUnknownMoE  TruncationKind = "unknown_moe"
)

// TruncationDecision is explicit about refusal: Allowed=false means callers
// must run the full model or leave the probe inconclusive.
type TruncationDecision struct {
	Allowed     bool           `json:"allowed"`
	Kind        TruncationKind `json:"kind"`
	TotalLayers int            `json:"total_layers"`
	MinLayers   int            `json:"min_layers,omitempty"`
	Reason      string         `json:"reason"`
}

func DecideTruncation(config ModelConfig) TruncationDecision {
	total := config.NumHiddenLayers
	if total <= 0 {
		return TruncationDecision{Kind: TruncationDense, Reason: "num_hidden_layers is missing; truncation refused"}
	}
	kind, minimum := TruncationDense, 2
	if config.FirstKDenseReplace != nil {
		kind = TruncationDeepSeekMoE
		if *config.FirstKDenseReplace < 0 {
			return refused(kind, total, "first_k_dense_replace must be non-negative")
		}
		if config.MoELayerFreq != nil && *config.MoELayerFreq <= 0 {
			return refused(kind, total, "moe_layer_freq must be positive")
		}
		minimum = max(minimum, *config.FirstKDenseReplace+2)
	} else if config.DecoderSparseStep != nil && positive(config.NumExperts) {
		kind = TruncationQwenMoE
		if *config.DecoderSparseStep <= 0 {
			return refused(kind, total, "decoder_sparse_step must be positive")
		}
		mlpOnly := make(map[int]struct{}, len(config.MLPOnlyLayers))
		for _, layer := range config.MLPOnlyLayers {
			mlpOnly[layer] = struct{}{}
		}
		firstMoE := -1
		for layer := 0; layer < total; layer++ {
			_, denseOnly := mlpOnly[layer]
			if !denseOnly && (layer+1)%*config.DecoderSparseStep == 0 {
				firstMoE = layer
				break
			}
		}
		if firstMoE < 0 {
			return refused(kind, total, "no eligible MoE layer found")
		}
		minimum = max(minimum, firstMoE+2)
	} else if hasMoESignal(config) {
		return refused(TruncationUnknownMoE, total, "MoE fields are present but the layer-selection rule is unknown")
	}

	if len(config.LayerTypes) > 0 {
		kind = TruncationHybrid
		if len(config.LayerTypes) != total {
			return refused(kind, total, "layer_types length does not match num_hidden_layers")
		}
		prefix, err := hybridPrefix(config.LayerTypes)
		if err != nil {
			return refused(kind, total, err.Error())
		}
		minimum = max(minimum, prefix+1)
	}
	minimum = min(minimum, total)
	return TruncationDecision{
		Allowed: true, Kind: kind, TotalLayers: total, MinLayers: minimum,
		Reason: fmt.Sprintf("shortest conservative prefix is %d of %d layers", minimum, total),
	}
}

func hybridPrefix(layerTypes []string) (int, error) {
	all := map[string]struct{}{}
	for _, layerType := range layerTypes {
		layerType = strings.TrimSpace(layerType)
		if layerType == "" {
			return 0, fmt.Errorf("layer_types contains an empty value")
		}
		all[layerType] = struct{}{}
	}
	seen := map[string]struct{}{}
	for index, layerType := range layerTypes {
		seen[strings.TrimSpace(layerType)] = struct{}{}
		if len(seen) == len(all) {
			return index + 1, nil
		}
	}
	return 0, fmt.Errorf("layer_types does not contain every declared layer type")
}

func hasMoESignal(config ModelConfig) bool {
	return positive(config.NumExperts) || positive(config.NRoutedExperts) || positive(config.NumLocalExperts)
}

func positive(value *int) bool { return value != nil && *value > 0 }

func refused(kind TruncationKind, total int, reason string) TruncationDecision {
	return TruncationDecision{Kind: kind, TotalLayers: total, Reason: reason + "; run full layers or remain inconclusive"}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
