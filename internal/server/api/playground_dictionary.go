package api

import (
	"embed"
	"fmt"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/pkg/localization"
)

var requiredPlaygroundLocales = []string{localization.English, localization.Chinese}

//go:embed locales/playground/*.json
var playgroundLocaleFiles embed.FS

type playgroundLocale struct {
	Parameters map[string]string `json:"parameters"`
	Presets    map[string]string `json:"presets"`
	// PresetUnavailable is keyed by preset, then by the reason it cannot be
	// honored: an engine that drops chat_template_kwargs and an undeclared
	// model family are different facts and get different texts.
	PresetUnavailable map[string]map[string]string            `json:"preset_unavailable"`
	StreamOptions     map[string]playgroundStreamOptionLocale `json:"stream_options"`
}

type playgroundStreamOptionLocale struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// playgroundThinkingKeyDisableThinking is the preset whose value depends on the
// model family: the chat-template field that turns thinking off comes from
// playgroundThinkingFamilies.
const playgroundThinkingKeyDisableThinking = "disable_thinking"

type playgroundPresetDefinition struct {
	Key string
	// Value is the extra-parameters patch of a preset every model can send.
	// A preset whose patch depends on the model leaves it nil and is filled in
	// by playgroundPresets.
	Value map[string]any
	// FamilyDependent marks a preset the server may serve without a value,
	// because it cannot be honored by every deployment. Its wire form then
	// carries the localized text that names what the user can add by hand.
	FamilyDependent bool
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
	// The chat-template field that stops thinking is the model family's, so
	// this preset carries no fixed value: playgroundPresets writes the one the
	// selected deployment declares, and leaves it disabled otherwise.
	{Key: playgroundThinkingKeyDisableThinking, FamilyDependent: true},
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

// validatePresetUnavailable requires one text per family-dependent preset and
// per reason it can be unavailable, and rejects texts nothing asks for.
func validatePresetUnavailable(language string, texts map[string]map[string]string) error {
	for _, definition := range playgroundPresetDefinitions {
		if !definition.FamilyDependent {
			if _, ok := texts[definition.Key]; ok {
				return fmt.Errorf("locale %s has unavailable text for %q, which every model can honor", language, definition.Key)
			}
			continue
		}
		reasons, ok := texts[definition.Key]
		if !ok {
			return fmt.Errorf("locale %s is missing unavailable text for %q", language, definition.Key)
		}
		for reason := range reasons {
			if !slices.Contains(playgroundThinkingUnavailableReasons, reason) {
				return fmt.Errorf("locale %s has unavailable text for %q reason %q", language, definition.Key, reason)
			}
		}
		for _, reason := range playgroundThinkingUnavailableReasons {
			if reasons[reason] == "" {
				return fmt.Errorf("locale %s is missing unavailable text for %q reason %q", language, definition.Key, reason)
			}
		}
	}
	return nil
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

func playgroundParameters() []dto.PlaygroundParameter {
	parameters := make([]dto.PlaygroundParameter, 0, len(playgroundParameterDefinitions))
	for _, definition := range playgroundParameterDefinitions {
		definition.Label = make(localization.Text, len(requiredPlaygroundLocales))
		for _, language := range requiredPlaygroundLocales {
			definition.Label[language] = playgroundLocales[language].Parameters[definition.Name]
		}
		parameters = append(parameters, definition)
	}
	return parameters
}

// playgroundPresets builds the preset buttons of one catalog model. Disable
// Thinking carries the chat-template field the deployment's model family
// declares, so the page can enable the button exactly when the request it
// writes would stop thinking; a model no family covers is served the same
// preset without a value and with the text naming the parameters a user can
// add by hand instead.
func playgroundPresets(thinking playgroundThinkingSwitch) []dto.PlaygroundExtraPreset {
	presets := make([]dto.PlaygroundExtraPreset, 0, len(playgroundPresetDefinitions))
	for _, definition := range playgroundPresetDefinitions {
		preset := dto.PlaygroundExtraPreset{Key: definition.Key, Label: playgroundPresetLabel(definition.Key), Value: definition.Value}
		if definition.FamilyDependent {
			// A family-dependent preset sends the family's chat-template field,
			// so it never carries a fixed value, and a deployment no family
			// covers gets none at all: a button that writes a parameter the
			// template ignores is the bug this table replaced.
			if thinking.declared() {
				preset.Value, preset.AppliesWhen = thinking.value(), thinking.Family
			} else {
				preset.Unavailable = playgroundPresetUnavailable(definition.Key, thinking.Unavailable)
			}
		}
		presets = append(presets, preset)
	}
	return presets
}

func playgroundPresetLabel(key string) localization.Text {
	label := make(localization.Text, len(requiredPlaygroundLocales))
	for _, language := range requiredPlaygroundLocales {
		label[language] = playgroundLocales[language].Presets[key]
	}
	return label
}

// playgroundPresetUnavailable is the text of a preset the current deployment
// cannot honor. The model reason names the chat_template_kwargs fields the
// declared families use, so the reader learns the parameter to add by hand
// instead of being told the model cannot stop thinking.
func playgroundPresetUnavailable(key, reason string) localization.Text {
	unavailable := make(localization.Text, len(requiredPlaygroundLocales))
	for _, language := range requiredPlaygroundLocales {
		text := playgroundLocales[language].PresetUnavailable[key][reason]
		unavailable[language] = strings.ReplaceAll(text, "{{examples}}", playgroundDisableThinkingExamples())
	}
	return unavailable
}
