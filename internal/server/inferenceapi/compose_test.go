package inferenceapi

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestComposeRequestUsesFourLayersAndForcesRoutedModel(t *testing.T) {
	op := Operation{Type: OperationAudioSpeech, RequestDefaults: map[string]any{"task_type": "Base", "response_format": "wav"}, Inputs: []InputField{{Name: "ref_audio", Kind: "audio", Encoding: "data_url", Required: true}, {Name: "ref_text", Kind: "text", Required: true}}}
	got, err := ComposeRequest(op, "served", map[string]any{"input": "hello", "model": "attacker"}, map[string]any{"ref_audio": "data:audio/wav;base64,AA==", "ref_text": "hello"}, json.RawMessage(`{"response_format":"mp3","model":"attacker","speed":1.2}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(got, &body)
	want := map[string]any{"input": "hello", "task_type": "Base", "response_format": "mp3", "ref_audio": "data:audio/wav;base64,AA==", "ref_text": "hello", "speed": 1.2, "model": "served"}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body=%v want=%v", body, want)
	}
	if _, err := ComposeRequest(op, "served", nil, map[string]any{"voice": "alloy"}, nil); err == nil {
		t.Fatal("undeclared input accepted")
	}
	if _, err := ComposeRequest(op, "served", nil, map[string]any{"ref_audio": []byte("raw")}, nil); err == nil {
		t.Fatal("invalid media value accepted")
	}
	dataURL, err := EncodeMediaInput(op.Inputs[0], "audio/wav", []byte("RIFF"))
	if err != nil || dataURL != "data:audio/wav;base64,UklGRg==" {
		t.Fatalf("dataURL=%s err=%v", dataURL, err)
	}
}
