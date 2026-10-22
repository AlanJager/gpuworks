package inferenceapi

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/localization"
)

func TestNormalizeRequestDeclarationsRejectsConflictingOrInvalidFields(t *testing.T) {
	for _, tc := range []struct {
		name, op, field string
		input           InputField
		defaults        map[string]any
	}{
		{"empty name", OperationChat, "name", InputField{Kind: "text"}, nil},
		{"reserved model", OperationChat, "model", InputField{Name: "model", Kind: "text"}, nil},
		{"primary input", OperationChat, "messages", InputField{Name: "messages", Kind: "json"}, nil},
		{"kind", OperationChat, "parameter", InputField{Name: "parameter", Kind: "boolean"}, nil},
		{"text encoding", OperationChat, "parameter", InputField{Name: "parameter", Kind: "text", Encoding: "data_url"}, nil},
		{"media encoding", OperationChat, "parameter", InputField{Name: "parameter", Kind: "audio", Encoding: "base64"}, nil},
		{"missing media encoding", OperationChat, "parameter", InputField{Name: "parameter", Kind: "audio"}, nil},
		{"unsupported multipart", OperationAudioSpeech, "parameter", InputField{Name: "parameter", Kind: "audio", Encoding: "multipart"}, nil},
		{"empty options", OperationChat, "parameter", InputField{Name: "parameter", Kind: "select"}, nil},
		{"unregistered voice discovery", OperationChat, "voice", InputField{Name: "voice", Kind: "select"}, nil},
		{"wrong select default", OperationChat, "parameter", InputField{Name: "parameter", Kind: "select", Options: []string{"yes"}, Default: "no"}, nil},
		{"nonstring select default", OperationChat, "parameter", InputField{Name: "parameter", Kind: "select", Options: []string{"yes"}, Default: 1}, nil},
		{"empty option", OperationChat, "parameter", InputField{Name: "parameter", Kind: "select", Options: []string{""}}, nil},
		{"duplicate option", OperationChat, "parameter", InputField{Name: "parameter", Kind: "select", Options: []string{"yes", "yes"}}, nil},
		{"nonselect options", OperationChat, "parameter", InputField{Name: "parameter", Kind: "text", Options: []string{"yes"}}, nil},
		{"number default", OperationChat, "parameter", InputField{Name: "parameter", Kind: "number", Default: "1"}, nil},
		{"text default", OperationChat, "parameter", InputField{Name: "parameter", Kind: "text", Default: true}, nil},
		{"nonfinite default", OperationChat, "parameter", InputField{Name: "parameter", Kind: "number", Default: math.NaN()}, nil},
		{"discovered default", OperationAudioSpeech, "voice", InputField{Name: "voice", Kind: "select", Default: "alloy"}, nil},
		{"model default", OperationChat, "model", InputField{Name: "parameter", Kind: "text"}, map[string]any{"model": "other"}},
		{"primary default", OperationChat, "messages", InputField{Name: "parameter", Kind: "text"}, map[string]any{"messages": []any{}}},
		{"input default collision", OperationChat, "parameter", InputField{Name: "parameter", Kind: "text"}, map[string]any{"parameter": "value"}},
		{"non JSON default", OperationChat, "request_defaults", InputField{Name: "parameter", Kind: "text"}, map[string]any{"value": math.Inf(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeContract(Contract{Operations: []Operation{{Type: tc.op, Inputs: []InputField{tc.input}, RequestDefaults: tc.defaults}}})
			if err == nil || !strings.Contains(err.Error(), tc.op) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error=%v must identify %s/%s", err, tc.op, tc.field)
			}
		})
	}
	_, err := NormalizeContract(Contract{Operations: []Operation{{Type: OperationChat, Inputs: []InputField{{Name: "extra", Kind: "text"}, {Name: "extra", Kind: "json"}}}}})
	if err == nil || !strings.Contains(err.Error(), "extra") {
		t.Fatalf("duplicate inputs error=%v", err)
	}
}

func TestRequestDeclarationNormalizationPreservesOrderAndOwnsCopies(t *testing.T) {
	input := Contract{Operations: []Operation{{Type: OperationAudioSpeech, RequestDefaults: map[string]any{"nested": map[string]any{"key": "original"}}, Inputs: []InputField{
		{Name: "voice", Kind: "select", Label: localization.Text{"en": "Voice", "zh-CN": "音色"}},
		{Name: "language", Kind: "select", Options: []string{"Auto", "Chinese"}, Default: "Auto"},
		{Name: "ref_audio", Kind: "audio", Encoding: "data_url"},
	}}}}
	got, err := NormalizeContract(input)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(got)
	roundTrip, ok := contractFromJSON(before)
	if !ok || !reflect.DeepEqual(got, roundTrip) {
		t.Fatalf("round trip=%+v", roundTrip)
	}
	got.Operations[0].RequestDefaults["nested"].(map[string]any)["key"] = "changed"
	got.Operations[0].Inputs[0].Label["en"] = "changed"
	got.Operations[0].Inputs[1].Options[0] = "changed"
	inputWire, _ := json.Marshal(input)
	if strings.Contains(string(inputWire), "changed") {
		t.Fatalf("input mutated: %s", inputWire)
	}
	legacy, ok := contractFromJSON(json.RawMessage(`{"operations":[{"type":"openai.chat"}]}`))
	if !ok || legacy.Operations[0].Inputs == nil || len(legacy.Operations[0].Inputs) != 0 {
		t.Fatalf("legacy inputs=%+v", legacy)
	}
	if _, err := NormalizeContract(Contract{Operations: []Operation{{Type: OperationAudioTranscribe, Inputs: []InputField{{Name: "reference", Kind: "file", Encoding: "multipart"}}}}}); err != nil {
		t.Fatal(err)
	}
}
