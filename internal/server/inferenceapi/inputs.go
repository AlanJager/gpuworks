package inferenceapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/pkg/localization"
)

// InputField is a model-owned parameter declaration. Primary protocol inputs
// and routing identity remain outside this list.
type InputField struct {
	Name     string            `yaml:"name" json:"name"`
	Kind     string            `yaml:"kind" json:"kind"`
	Encoding string            `yaml:"encoding,omitempty" json:"encoding,omitempty"`
	Required bool              `yaml:"required,omitempty" json:"required,omitempty"`
	Label    localization.Text `yaml:"label,omitempty" json:"label,omitempty"`
	Options  []string          `yaml:"options,omitempty" json:"options,omitempty"`
	Default  any               `yaml:"default,omitempty" json:"default,omitempty"`
}

func normalizeRequestDeclaration(operation *Operation, definition Definition) error {
	reserved := map[string]bool{"model": true}
	for _, name := range definition.PrimaryInputs {
		reserved[name] = true
	}
	operation.Inputs = append([]InputField{}, operation.Inputs...)
	for index := range operation.Inputs {
		field := &operation.Inputs[index]
		field.Name = strings.TrimSpace(field.Name)
		if field.Name == "" {
			return fmt.Errorf("inputs[%d].name must not be empty", index)
		}
		if reserved[field.Name] {
			return fmt.Errorf("inputs.%s name conflicts with model, a primary input, or another input", field.Name)
		}
		reserved[field.Name] = true
		field.Kind = strings.ToLower(strings.TrimSpace(field.Kind))
		if !contains([]string{"text", "number", "select", "json", "audio", "image", "file"}, field.Kind) {
			return fmt.Errorf("inputs.%s has invalid kind %q", field.Name, field.Kind)
		}
		media := field.Kind == "audio" || field.Kind == "image" || field.Kind == "file"
		field.Encoding = strings.ToLower(strings.TrimSpace(field.Encoding))
		if media {
			if field.Encoding != "data_url" && field.Encoding != "multipart" {
				return fmt.Errorf("inputs.%s requires encoding data_url or multipart", field.Name)
			}
			if field.Encoding == "multipart" && !contains(definition.RequestEncodings, EncodingMultipart) {
				return fmt.Errorf("inputs.%s uses multipart on an operation without multipart encoding", field.Name)
			}
		} else if field.Encoding != "" {
			return fmt.Errorf("inputs.%s encoding is only valid for media kinds", field.Name)
		}
		field.Options = append([]string(nil), field.Options...)
		if field.Kind == "select" {
			if len(field.Options) == 0 && !hasInputDiscovery(definition, field.Name) {
				return fmt.Errorf("inputs.%s select requires nonempty options or registered discovery", field.Name)
			}
			seen := map[string]bool{}
			for _, option := range field.Options {
				if strings.TrimSpace(option) == "" || seen[option] {
					return fmt.Errorf("inputs.%s options must be nonempty and unique", field.Name)
				}
				seen[option] = true
			}
		} else if len(field.Options) != 0 {
			return fmt.Errorf("inputs.%s options are only valid for select", field.Name)
		}
		if field.Default != nil {
			value, err := cloneJSONValue(field.Default)
			if err != nil {
				return fmt.Errorf("inputs.%s default must be JSON: %w", field.Name, err)
			}
			field.Default = value
			if err := validateInputValue(*field, value); err != nil {
				return fmt.Errorf("inputs.%s default: %w", field.Name, err)
			}
			if field.Kind == "select" && len(field.Options) == 0 {
				return fmt.Errorf("inputs.%s default must belong to options", field.Name)
			}
		}
		if field.Label != nil {
			label := localization.Text{}
			for language, text := range field.Label {
				label[language] = text
			}
			field.Label = label
		}
	}
	for name := range operation.RequestDefaults {
		if reserved[name] {
			return fmt.Errorf("request_defaults.%s conflicts with model, a primary input, or inputs.name", name)
		}
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("request_defaults keys must not be empty")
		}
	}
	if len(operation.ExampleInput) > 0 {
		example := make(map[string]string, len(operation.ExampleInput))
		for name, value := range operation.ExampleInput {
			if !textPrimaryInput(definition, name) {
				return fmt.Errorf("example_input.%s is not a text primary input of this operation", name)
			}
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("example_input.%s must not be empty", name)
			}
			example[name] = value // kept as written: a format may end in a newline
		}
		operation.ExampleInput = example
	}
	if operation.RequestDefaults != nil {
		value, err := cloneJSONValue(operation.RequestDefaults)
		if err != nil {
			return fmt.Errorf("request_defaults must be a JSON object: %w", err)
		}
		operation.RequestDefaults = value.(map[string]any)
	}
	return nil
}

func hasInputDiscovery(definition Definition, name string) bool {
	// The protocol registry owns which runtime fact supplies which input.
	if name != "voice" {
		return false
	}
	for _, route := range definition.Discovery {
		if route.Name == "voices" {
			return true
		}
	}
	return false
}

func validateInputValue(field InputField, value any) error {
	switch field.Kind {
	case "text", "audio", "image", "file":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("must be a string")
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("must be a number")
		}
	case "select":
		text, ok := value.(string)
		if !ok || len(field.Options) > 0 && !contains(field.Options, text) {
			return fmt.Errorf("must belong to options")
		}
	}
	return nil
}

func cloneJSONValue(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result any
	err = json.Unmarshal(encoded, &result)
	return result, err
}

// textPrimaryInputs are the primary inputs whose value is one text: the ones
// example_input may give a working value for.
var textPrimaryInputs = map[string]bool{"prompt": true, "input": true, "query": true}

func textPrimaryInput(definition Definition, name string) bool {
	return textPrimaryInputs[name] && contains(definition.PrimaryInputs, name)
}

// exampleText is the declared example value of a text primary input, else
// fallback.
func exampleText(operation Operation, name, fallback string) string {
	if value, ok := operation.ExampleInput[name]; ok {
		return value
	}
	return fallback
}
