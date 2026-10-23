package api

import (
	"embed"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/pkg/localization"
)

var requiredPlaygroundLocales = []string{localization.English, localization.Chinese}

//go:embed locales/playground/*.json
var playgroundLocaleFiles embed.FS

type playgroundLocale struct {
	Parameters        map[string]string                       `json:"parameters"`
	Presets           map[string]string                       `json:"presets"`
	PresetUnavailable map[string]string                       `json:"preset_unavailable"`
	StreamOptions     map[string]playgroundStreamOptionLocale `json:"stream_options"`
}

type playgroundStreamOptionLocale struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// playgroundThinkingToggle is the only model family whose "disable thinking"
// preset is a reliable switch. Other presets stay generic parameter examples.
const playgroundThinkingToggle = "qwen3"

type playgroundPresetDefinition struct {
	Key         string
	Value       map[string]any
	AppliesWhen string
}

var playgroundParameterDefinitions = []dto.PlaygroundParameter{
	{Name: "temperature", Kind: "number", Minimum: 0, Maximum: 2, Step: 0.05, Default: 0.7},
	{Name: "top_p", Kind: "number", Minimum: 0, Maximum: 1, Step: 0.05, Default: 0.9},
	{Name: "max_tokens", Kind: "integer", Minimum: 1, Maximum: 32768, Step: 1, Default: 1024},
	{Name: "seed", Kind: "integer", Minimum: 0, Maximum: 2147483647, Step: 1, Default: 0},
	{Name: "stop", Kind: "string", Default: ""},
	{Name: "frequency_penalty", Kind: "number", Minimum: -2, Maximum: 2, Step: 0.1, Default: 0},
	{Name: "presence_penalty", Kind: "number", Minimum: -2, Maximum: 2, Step: 0.1, Default: 0},
}

// playgroundStreamOptionDefinitions are on by default. include_usage gives
// the final token counts; continuous_usage_stats (vLLM, SGLang) puts the
// running counts on every chunk, so speed metrics update while the reply
// streams instead of only at its end. Engines only honour the latter with
// include_usage on.
var playgroundStreamOptionDefinitions = []dto.PlaygroundStreamOption{
	{Name: "include_usage", Default: true},
	{Name: "continuous_usage_stats", Default: true, Requires: "include_usage"},
}

var playgroundPresetDefinitions = []playgroundPresetDefinition{
	{Key: "low_reasoning_effort", Value: map[string]any{"reasoning_effort": "low"}},
	// vLLM and SGLang apply Qwen3's chat-template flag only through
	// chat_template_kwargs. A top-level enable_thinking field is accepted and
	// ignored, so the model keeps writing the thinking body.
	{Key: "disable_thinking", AppliesWhen: playgroundThinkingToggle, Value: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}},
}

var playgroundLocales map[string]playgroundLocale

func init() {
	locales, err := localization.LoadJSONFiles[playgroundLocale](playgroundLocaleFiles, "locales/playground/%s.json", requiredPlaygroundLocales)
	if err == nil {
		err = validatePlaygroundLocales(locales)
	}
	if err != nil {
		panic(fmt.Sprintf("load playground locales: %v", err))
	}
	playgroundLocales = locales
}

func validatePlaygroundLocales(locales map[string]playgroundLocale) error {
	parameterKeys := make(map[string]struct{}, len(playgroundParameterDefinitions))
	for _, definition := range playgroundParameterDefinitions {
		parameterKeys[definition.Name] = struct{}{}
	}
	presetKeys := make(map[string]struct{}, len(playgroundPresetDefinitions))
	for _, definition := range playgroundPresetDefinitions {
		presetKeys[definition.Key] = struct{}{}
	}
	for _, language := range requiredPlaygroundLocales {
		locale, ok := locales[language]
		if !ok {
			return fmt.Errorf("missing required locale %q", language)
		}
		if err := validateDisplayLabels(language, "parameter", parameterKeys, locale.Parameters); err != nil {
			return err
		}
		if err := validateDisplayLabels(language, "preset", presetKeys, locale.Presets); err != nil {
			return err
		}
		if err := validatePresetUnavailable(language, locale.PresetUnavailable); err != nil {
			return err
		}
		if len(locale.StreamOptions) != len(playgroundStreamOptionDefinitions) {
			return fmt.Errorf("locale %s has %d stream options, want %d", language, len(locale.StreamOptions), len(playgroundStreamOptionDefinitions))
		}
		for _, definition := range playgroundStreamOptionDefinitions {
			if entry := locale.StreamOptions[definition.Name]; entry.Label == "" || entry.Description == "" {
				return fmt.Errorf("locale %s is missing stream option %q", language, definition.Name)
			}
		}
	}
	return nil
}

func validateDisplayLabels(language, kind string, expected map[string]struct{}, labels map[string]string) error {
	for key := range labels {
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("locale %s contains unknown %s %q", language, kind, key)
		}
	}
	for key := range expected {
		if labels[key] == "" {
			return fmt.Errorf("locale %s is missing %s %q", language, kind, key)
		}
	}
	return nil
}

func validatePresetUnavailable(language string, labels map[string]string) error {
	expected := map[string]struct{}{}
	for _, definition := range playgroundPresetDefinitions {
		if definition.AppliesWhen != "" {
			expected[definition.Key] = struct{}{}
		}
	}
	return validateDisplayLabels(language, "preset unavailable text", expected, labels)
}

func playgroundStreamOptions() []dto.PlaygroundStreamOption {
	options := make([]dto.PlaygroundStreamOption, 0, len(playgroundStreamOptionDefinitions))
	for _, definition := range playgroundStreamOptionDefinitions {
		definition.Label = make(localization.Text, len(requiredPlaygroundLocales))
		definition.Description = make(localization.Text, len(requiredPlaygroundLocales))
		for _, language := range requiredPlaygroundLocales {
			entry := playgroundLocales[language].StreamOptions[definition.Name]
			definition.Label[language], definition.Description[language] = entry.Label, entry.Description
		}
		options = append(options, definition)
	}
	return options
}

func playgroundDictionary() ([]dto.PlaygroundParameter, []dto.PlaygroundExtraPreset) {
	parameters := make([]dto.PlaygroundParameter, 0, len(playgroundParameterDefinitions))
	for _, definition := range playgroundParameterDefinitions {
		definition.Label = make(localization.Text, len(requiredPlaygroundLocales))
		for _, language := range requiredPlaygroundLocales {
			definition.Label[language] = playgroundLocales[language].Parameters[definition.Name]
		}
		parameters = append(parameters, definition)
	}
	presets := make([]dto.PlaygroundExtraPreset, 0, len(playgroundPresetDefinitions))
	for _, definition := range playgroundPresetDefinitions {
		label := make(localization.Text, len(requiredPlaygroundLocales))
		for _, language := range requiredPlaygroundLocales {
			label[language] = playgroundLocales[language].Presets[definition.Key]
		}
		preset := dto.PlaygroundExtraPreset{Key: definition.Key, Label: label, Value: definition.Value, AppliesWhen: definition.AppliesWhen}
		if definition.AppliesWhen != "" {
			preset.Unavailable = make(localization.Text, len(requiredPlaygroundLocales))
			for _, language := range requiredPlaygroundLocales {
				preset.Unavailable[language] = playgroundLocales[language].PresetUnavailable[definition.Key]
			}
		}
		presets = append(presets, preset)
	}
	return parameters, presets
}
