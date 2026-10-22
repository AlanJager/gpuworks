package aihelper

import "testing"

func TestDeepSeekPresetUsesOfficialDefaults(t *testing.T) {
	p, ok := Presets["deepseek"]
	if !ok {
		t.Fatal("deepseek preset is missing")
	}
	if p.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("base URL = %q, want https://api.deepseek.com", p.BaseURL)
	}
	if p.Model != "deepseek-v4-flash" {
		t.Fatalf("model = %q, want deepseek-v4-flash", p.Model)
	}
}
