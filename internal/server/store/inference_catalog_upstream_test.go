package store

import "testing"

// Saving the template editor sends the whole model back. A user spec added to
// an upstream template does not take over the model's estimation facts: the
// modalities, model config and KV cost stay what the catalog declared, even
// when the saved copy carried other values or none.
func TestUserImportKeepsAnUpstreamModelsEstimationFacts(t *testing.T) {
	st := openObservationStore(t)
	upstream := InferenceModel{
		ModelID: "org/up", Source: "upstream", InputModalities: `["text","image"]`,
		ModelConfig: `{"num_hidden_layers":48}`, KVBytesPerToken: `{"bf16":98304}`,
		Specs: observationModel("x", "").Specs,
	}
	upstream.Specs[0].Source = "upstream"
	if err := st.ReconcileInferenceCatalog([]InferenceModel{upstream}); err != nil {
		t.Fatal(err)
	}
	edited := observationModel("org/up", "")
	edited.Specs[0].Name = "mine"
	edited.InputModalities, edited.ModelConfig, edited.KVBytesPerToken = `["text"]`, "", ""
	if err := st.ImportUserInferenceCatalog([]InferenceModel{edited}, true); err != nil {
		t.Fatal(err)
	}
	got, err := st.InferenceModelByModelID("org/up")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "upstream" || got.InputModalities != upstream.InputModalities || got.ModelConfig != upstream.ModelConfig || got.KVBytesPerToken != upstream.KVBytesPerToken {
		t.Fatalf("model facts = source %q modalities %q config %q kv %q", got.Source, got.InputModalities, got.ModelConfig, got.KVBytesPerToken)
	}
	names := map[string]string{}
	for _, spec := range got.Specs {
		names[spec.Name] = spec.Source
	}
	if names["general"] != "upstream" || names["mine"] != "user" {
		t.Fatalf("specs = %v", names)
	}
}
