package aihelper

import (
	"strings"
	"testing"
)

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
		{"http://198.51.100.122:8800/v1/chat/completions", "http://198.51.100.122:8800/v1", true},
		{"http://198.51.100.122:8800/v1/chat/completions/", "http://198.51.100.122:8800/v1", true},
		{"https://api.deepseek.com/chat/completions", "https://api.deepseek.com", true},
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1", true},
		// The suffix is matched case-insensitively, because a pasted address is
		// not a place to be strict about case.
		{"https://host/v1/CHAT/Completions", "https://host/v1", true},
		// Pasted twice: both copies go, so the stored base plus the call path
		// is one call path and no more. Leaving one copy behind would also
		// break idempotency - normalizing the result again would strip the
		// second copy, and a form that saves an untouched address must not
		// keep rewriting it.
		{"http://host/chat/completions/chat/completions", "http://host", true},
		{"http://host/v1/chat/completions/chat/completions", "http://host/v1", true},
		// Only the call path is dropped, not a path that merely contains it.
		{"https://host/v1/chat/completions/extra", "https://host/v1/chat/completions/extra", false},
		{"https://host/v1beta", "https://host/v1beta", false},
		{"https://host/v1/xchat/completions", "https://host/v1/xchat/completions", false},
		{"", "", false},
		{"   ", "", true},
		{"/v1", "/v1", false},

		// A query is part of the base address, not part of the call: Azure
		// serves ".../openai/deployments/<name>?api-version=<date>" and
		// dropping that query would store an address that cannot be called.
		{"https://h/openai/deployments/foo/chat/completions?api-version=2024-02-01", "https://h/openai/deployments/foo?api-version=2024-02-01", true},
		{"https://h/openai/deployments/foo?api-version=2024-02-01", "https://h/openai/deployments/foo?api-version=2024-02-01", false},
		// A query the operator put on the pasted call path is kept, and
		// ChatCompletionsURL is what puts it back on the call.
		{"https://h/v1/chat/completions?x=1", "https://h/v1?x=1", true},
		{"https://h/v1?x=1", "https://h/v1?x=1", false},
		{"https://h/v1/chat/completions/?x=1", "https://h/v1?x=1", true},
		// A fragment is kept for the same reason and reaches no server.
		{"https://h/v1/chat/completions#frag", "https://h/v1#frag", true},
		{"https://h/v1#frag", "https://h/v1#frag", false},
		{"https://h/v1/chat/completions?x=1#frag", "https://h/v1?x=1#frag", true},
	} {
		got, moved := NormalizeBaseURL(tc.raw)
		if got != tc.want || moved != tc.moved {
			t.Errorf("NormalizeBaseURL(%q) = %q, %t; want %q, %t", tc.raw, got, moved, tc.want, tc.moved)
		}
		// Normalizing what was just normalized must change nothing, or saving
		// an untouched address through the form would rewrite the row again.
		if again, movedAgain := NormalizeBaseURL(got); movedAgain || again != got {
			t.Errorf("NormalizeBaseURL(%q) = %q, %t; want %q, false (idempotent)", got, again, movedAgain, got)
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

// The invariant normalization exists to keep: for every address shape an
// operator may paste, the stored base plus the call path is a sane call URL -
// one call path, and a base query on the call rather than inside its path.
func TestChatCompletionsURL(t *testing.T) {
	for _, tc := range []struct {
		base string
		want string
	}{
		{"https://api.deepseek.com", "https://api.deepseek.com/chat/completions"},
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/v1/", "https://api.deepseek.com/v1/chat/completions"},
		// A row stored before normalization existed still calls correctly.
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		// The reason the call is built from the parsed address: a base query
		// goes with the call instead of ending up inside its path.
		{"https://h/openai/deployments/foo?api-version=2024-02-01", "https://h/openai/deployments/foo/chat/completions?api-version=2024-02-01"},
		{"https://h/v1?x=1", "https://h/v1/chat/completions?x=1"},
	} {
		got, err := ChatCompletionsURL(tc.base)
		if err != nil {
			t.Fatalf("ChatCompletionsURL(%q): %v", tc.base, err)
		}
		if got != tc.want {
			t.Errorf("ChatCompletionsURL(%q) = %q; want %q", tc.base, got, tc.want)
		}
		if n := strings.Count(strings.SplitN(got, "?", 2)[0], "/chat/completions"); n != 1 {
			t.Errorf("ChatCompletionsURL(%q) = %q; want exactly one call path, got %d", tc.base, got, n)
		}
	}
}
