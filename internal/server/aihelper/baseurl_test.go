package aihelper

import "testing"

// An operator pastes whatever the provider's documentation shows. The stored
// address is the one a client is given, not the call it makes: a pasted call
// path is dropped so the client's own append does not double it.
func TestNormalizeBaseURL(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  string
		moved bool
	}{
		{"https://api.deepseek.com", "https://api.deepseek.com", false},
		{"https://api.deepseek.com/", "https://api.deepseek.com", true},
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1", false},
		{"https://api.deepseek.com/v1/", "https://api.deepseek.com/v1", true},
		{"https://api.deepseek.com/v1///", "https://api.deepseek.com/v1", true},
		{"  https://api.deepseek.com/v1  ", "https://api.deepseek.com/v1", true},
		// The case the bug reported: the whole call path pasted in.
		{"http://172.30.8.122:8800/v1/chat/completions", "http://172.30.8.122:8800/v1", true},
		{"http://172.30.8.122:8800/v1/chat/completions/", "http://172.30.8.122:8800/v1", true},
		{"https://api.deepseek.com/chat/completions", "https://api.deepseek.com", true},
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1", true},
		// A base that ends in the path is left with a base to append to.
		{"http://host/chat/completions/chat/completions", "http://host/chat/completions", true},
		// Only the call path is dropped, not a path that merely contains it.
		{"https://host/v1/chat/completions/extra", "https://host/v1/chat/completions/extra", false},
		{"https://host/v1beta", "https://host/v1beta", false},
		{"", "", false},
		{"   ", "", true},
		{"/v1", "/v1", false},
	} {
		got, moved := NormalizeBaseURL(tc.raw)
		if got != tc.want || moved != tc.moved {
			t.Errorf("NormalizeBaseURL(%q) = %q, %t; want %q, %t", tc.raw, got, moved, tc.want, tc.moved)
		}
	}
}

// The stored presets are already base addresses: normalizing one must not
// change it, or an edit that never touched the address would rewrite it.
func TestNormalizeBaseURLLeavesPresetsAlone(t *testing.T) {
	for name, preset := range Presets {
		if got, moved := NormalizeBaseURL(preset.BaseURL); moved || got != preset.BaseURL {
			t.Errorf("preset %s: NormalizeBaseURL(%q) = %q, %t", name, preset.BaseURL, got, moved)
		}
	}
}
