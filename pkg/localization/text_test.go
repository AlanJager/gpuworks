package localization

import (
	"encoding/json"
	"testing"
)

func TestTextUnmarshalSupportsLegacyStringAndLanguageMap(t *testing.T) {
	for name, raw := range map[string]string{
		"legacy": `"Legacy"`,
		"map":    `{"en":"English","zh":"Chinese"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var text Text
			if err := json.Unmarshal([]byte(raw), &text); err != nil {
				t.Fatal(err)
			}
			if text[English] == "" {
				t.Fatalf("English fallback missing: %#v", text)
			}
		})
	}
}
