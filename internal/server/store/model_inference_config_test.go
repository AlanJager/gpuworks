package store

import "testing"

func TestOperationsUnset(t *testing.T) {
	for raw, want := range map[string]bool{
		"":                            true,
		"  \n":                        true,
		"[]":                          true,
		" [ ] ":                       true,
		"[\n]":                        true,
		"[\t]":                        true,
		`[{"type":"openai.chat"}]`:    false,
		`[ {"type":"openai.chat"} ]`:  false,
		"{not json":                   false,
		`{"type":"openai.chat"}`:      false,
		"null":                        true,
		`[{"type":"openai.invalid"}]`: false,
	} {
		if got := OperationsUnset(raw); got != want {
			t.Errorf("OperationsUnset(%q)=%v want %v", raw, got, want)
		}
	}
}

// Every spelling of "no operations" is filled by the source's declaration, and
// the SQL condition agrees with OperationsUnset on each of them.
func TestInitModelInferenceConfigFillsEmptyOperations(t *testing.T) {
	declared := `[{"type":"openai.chat"}]`
	for name, stored := range map[string]string{
		"empty": "", "empty array": "[]", "null": "null", "spaced array": "[ ]", "newline array": "[\n]", "blank": "  ",
	} {
		t.Run(name, func(t *testing.T) {
			st := newTestStore(t)
			model, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "acme/" + name, Revision: "main", Name: name, Operations: stored})
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.InitModelInferenceConfig(model.ID, declared, "")
			if err != nil || got.Operations != declared {
				t.Fatalf("operations=%q err=%v want %q", got.Operations, err, declared)
			}
		})
	}
}

// Operations that are present stay, and a value a concurrent edit writes
// between the caller's read and Init's UPDATE wins: the condition is in the
// UPDATE, not in the caller's view of the row.
func TestInitModelInferenceConfigKeepsNonEmptyOperations(t *testing.T) {
	st := newTestStore(t)
	model, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "acme/kept", Revision: "main", Name: "kept", Operations: "[]", InputModalities: `["text"]`})
	if err != nil {
		t.Fatal(err)
	}
	// The caller saw [] ...
	if !OperationsUnset(model.Operations) {
		t.Fatalf("precondition: %q", model.Operations)
	}
	// ... then an edit lands a real value before the deploy's write.
	edited := `[{"type":"openai.embeddings"}]`
	if _, err := st.SetModelInferenceConfig(model.ID, edited, `["text"]`); err != nil {
		t.Fatal(err)
	}
	got, err := st.InitModelInferenceConfig(model.ID, `[{"type":"openai.chat"}]`, `["text","image"]`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Operations != edited || got.InputModalities != `["text"]` {
		t.Fatalf("a concurrent value was overwritten: operations=%q modalities=%q", got.Operations, got.InputModalities)
	}
}
