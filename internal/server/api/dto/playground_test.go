package dto

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/localization"
)

// PlaygroundCatalog wire contract. The extra-parameter presets travel with the
// model, because whether a preset can be honored depends on the deployment's
// engine and model family: a preset with a value is applicable, one without a
// value carries the text that says what to add by hand. The page reads exactly
// these fields, so they are pinned here instead of only through handler tests.
func TestPlaygroundCatalogPresetWireContract(t *testing.T) {
	catalog := PlaygroundCatalog{
		Models: []PlaygroundModel{{
			ID: "deepseek-ai/DeepSeek-V4.1-Flash-1017-zdji", Label: "DeepSeek-V4.1-Flash-1017", Sleeping: false, PublicDataPlane: true,
			Operations: []PlaygroundOperation{{Type: "openai.chat"}},
			ExtraParameterPresets: []PlaygroundExtraPreset{
				{Key: "low_reasoning_effort", Label: localization.Text{"en": "Low Reasoning Effort", "zh": "低推理强度"}, Value: map[string]any{"reasoning_effort": "low"}},
				{Key: "disable_thinking", Label: localization.Text{"en": "Disable Thinking", "zh": "关闭思考"}, AppliesWhen: "deepseek-v4", Value: map[string]any{"chat_template_kwargs": map[string]any{"thinking": false}}},
			},
		}},
		Parameters:    []PlaygroundParameter{},
		StreamOptions: []PlaygroundStreamOption{},
	}
	wire, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"extra_parameter_presets":[{"key":"low_reasoning_effort","label":{"en":"Low Reasoning Effort","zh":"低推理强度"},"value":{"reasoning_effort":"low"}}`,
		`{"key":"disable_thinking","label":{"en":"Disable Thinking","zh":"关闭思考"},"value":{"chat_template_kwargs":{"thinking":false}},"applies_when":"deepseek-v4"}`,
	} {
		if !strings.Contains(string(wire), want) {
			t.Fatalf("wire =\n%s\nwant %s", wire, want)
		}
	}
	// No flat catalog-level preset list and no boolean toggle: both said the
	// preset applies to every listed model, which is the bug this shape fixes.
	for _, gone := range []string{`"thinking_toggle"`} {
		if strings.Contains(string(wire), gone) {
			t.Fatalf("wire still carries %s: %s", gone, wire)
		}
	}
	var back PlaygroundCatalog
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Models) != 1 || len(back.Models[0].ExtraParameterPresets) != 2 {
		t.Fatalf("round trip = %+v", back.Models)
	}
	if back.Models[0].ExtraParameterPresets[1].AppliesWhen != "deepseek-v4" {
		t.Fatalf("round trip lost applies_when: %+v", back.Models[0].ExtraParameterPresets[1])
	}
	kwargs, ok := back.Models[0].ExtraParameterPresets[1].Value["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["thinking"] != false {
		t.Fatalf("round trip lost the patch: %#v", back.Models[0].ExtraParameterPresets[1].Value)
	}
}

// An unavailable preset is the other half of the contract: no value, so a page
// cannot apply it, plus the localized text naming what to add by hand.
func TestPlaygroundUnavailablePresetWireContract(t *testing.T) {
	preset := PlaygroundExtraPreset{
		Key:         "disable_thinking",
		Label:       localization.Text{"en": "Disable Thinking"},
		Unavailable: localization.Text{"en": "Add the parameter yourself under Extra Parameters, for example {\"chat_template_kwargs\":{\"thinking\":false}}."},
	}
	wire, err := json.Marshal(preset)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), `"value"`) || strings.Contains(string(wire), `"applies_when"`) {
		t.Fatalf("an unavailable preset must carry no value or capability: %s", wire)
	}
	if !strings.Contains(string(wire), `"unavailable":{"en":"Add the parameter yourself`) {
		t.Fatalf("wire = %s", wire)
	}
	var back PlaygroundExtraPreset
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	if back.Value != nil || back.Unavailable[localization.English] == "" {
		t.Fatalf("round trip = %+v", back)
	}
}
