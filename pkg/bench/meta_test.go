package bench

import (
	"encoding/json"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/pkg/localization"
)

func hasRequiredBenchmarkText(text localization.Text) bool {
	for _, language := range requiredBenchmarkLocales {
		if text[language] == "" {
			return false
		}
	}
	return true
}

func TestDictionaryHasCompleteLocalizedTextAndStableValues(t *testing.T) {
	meta := Dictionary()
	for section, options := range map[string][]Option{
		"modes": meta.Modes, "datasets": meta.Datasets, "retired datasets": meta.RetiredDatasets, "objectives": meta.Objectives, "statuses": meta.Statuses,
	} {
		for _, option := range options {
			if option.Value == "" || !hasRequiredBenchmarkText(option.Label) {
				t.Errorf("%s contains an incomplete option: %#v", section, option)
			}
			if (section == "modes" || section == "datasets") && !hasRequiredBenchmarkText(option.Description) {
				t.Errorf("%s option %q has an incomplete description: %#v", section, option.Value, option.Description)
			}
		}
	}
	for _, field := range meta.SLAConstraints {
		if field.Key == "" || !hasRequiredBenchmarkText(field.Label) {
			t.Errorf("incomplete SLA constraint: %#v", field)
		}
	}

	modeValues := make([]string, 0, len(meta.Modes))
	for _, option := range meta.Modes {
		modeValues = append(modeValues, option.Value)
	}
	if want := []string{"saturation", "sla", "grid"}; !slices.Equal(modeValues, want) {
		t.Fatalf("mode values = %v, want %v", modeValues, want)
	}
	datasetValues := make([]string, 0, len(meta.Datasets))
	for _, option := range meta.Datasets {
		datasetValues = append(datasetValues, option.Value)
	}
	if want := []string{"random", "agentic", "multiturn", "imported"}; !slices.Equal(datasetValues, want) {
		t.Fatalf("dataset values = %v, want %v", datasetValues, want)
	}
	if len(meta.AgenticPresets) != 8 || meta.AgenticDefaults.Preset != "agentic-80k-v1" || meta.AgenticDefaults.SessionsPerPoint["8"] != 16 || meta.AgenticDefaults.SessionsPerConcurrency != 2 || meta.AgenticDefaults.MinSessionsPerPoint != 4 {
		t.Fatalf("agentic metadata is incomplete: %#v", meta)
	}
	for mode, defaults := range meta.ModeDefaults {
		if len(defaults.ISL) != 1 || defaults.ISL[0] != DefaultInputTokens || len(defaults.OSL) != 1 || defaults.OSL[0] != DefaultOutputTokens {
			t.Fatalf("%s token defaults changed: %#v", mode, defaults)
		}
	}
	if meta.ModeDefaults["saturation"].MaxConcurrency != DefaultMaxConcurrency || meta.DetailCapMB.Default != DefaultDetailCapMB {
		t.Fatalf("benchmark defaults changed: %#v", meta)
	}
}

func TestDictionaryJSONUsesMultilingualMaps(t *testing.T) {
	raw, err := json.Marshal(Dictionary())
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Modes []struct {
			Label       map[string]string `json:"label"`
			Description map[string]string `json:"description"`
		} `json:"modes"`
		SLAConstraints []struct {
			Label map[string]string `json:"label"`
		} `json:"sla_constraints"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Modes) == 0 || len(wire.SLAConstraints) == 0 {
		t.Fatalf("benchmark metadata is empty: %s", raw)
	}
	if !hasRequiredBenchmarkText(wire.Modes[0].Label) || !hasRequiredBenchmarkText(wire.Modes[0].Description) || !hasRequiredBenchmarkText(wire.SLAConstraints[0].Label) {
		t.Fatalf("unexpected multilingual benchmark wire contract: %s", raw)
	}
}

func TestValidateBenchmarkLocalesRejectsMissingTranslation(t *testing.T) {
	broken := make(map[string]benchmarkLocale, len(benchmarkLocales))
	for language, locale := range benchmarkLocales {
		broken[language] = locale
	}
	chinese := broken[localization.Chinese]
	chinese.Statuses = make(map[string]benchmarkLocaleEntry, len(chinese.Statuses))
	for key, entry := range benchmarkLocales[localization.Chinese].Statuses {
		if key != "failed" {
			chinese.Statuses[key] = entry
		}
	}
	broken[localization.Chinese] = chinese
	if err := validateBenchmarkLocales(dictionaryDefinition(), broken); err == nil {
		t.Fatal("missing benchmark status translation was accepted")
	}
}
