package inferenceapi

import "testing"

func TestNormalizeModelConfigCanonicalizesAndAllowsEmptyOperations(t *testing.T) {
	ops, modalities, err := NormalizeModelConfig([]Operation{{Type: " OPENAI.CHAT "}}, []string{" IMAGE ", "text", "image"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Type != OperationChat || ops[0].Endpoint != "/v1/chat/completions" || ops[0].Mode != "stream" {
		t.Fatalf("normalized operations = %+v", ops)
	}
	if got, want := modalities, []string{"image", "text"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("normalized modalities = %v, want %v", got, want)
	}
	ops, modalities, err = NormalizeModelConfig(nil, nil)
	if err != nil || ops != nil || modalities == nil {
		t.Fatalf("empty config = ops %v modalities %v err %v", ops, modalities, err)
	}
}

func TestNormalizeModelConfigRejectsUnsupportedValuesAndOperations(t *testing.T) {
	if _, _, err := NormalizeModelConfig([]Operation{{Type: "openai.nope"}}, []string{"text"}); err == nil {
		t.Fatal("unsupported operation accepted")
	}
	if _, _, err := NormalizeModelConfig([]Operation{{Type: OperationChat}}, []string{"document"}); err == nil {
		t.Fatal("unsupported input modality accepted")
	}
}
