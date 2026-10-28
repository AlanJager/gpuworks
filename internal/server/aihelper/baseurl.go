package aihelper

import "strings"

// chatCompletionsPath is the call every OpenAI-compatible client appends to
// Endpoint.BaseURL.
const chatCompletionsPath = "/chat/completions"

// NormalizeBaseURL answers the address an OpenAI-compatible client is given,
// not the call it makes. Operators paste whatever their provider's
// documentation shows, and showing the whole call path is common - a client
// that then appends "/chat/completions" asks
// ".../v1/chat/completions/chat/completions" and gets 404. The call path is
// dropped and trailing slashes trimmed, so ".../v1/chat/completions" becomes
// ".../v1". A bare host (some providers serve the path from the root) is left
// alone. The bool reports a change, so a form can say what was saved.
func NormalizeBaseURL(raw string) (string, bool) {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if len(base) >= len(chatCompletionsPath) && strings.EqualFold(base[len(base)-len(chatCompletionsPath):], chatCompletionsPath) {
		base = strings.TrimRight(base[:len(base)-len(chatCompletionsPath)], "/")
	}
	return base, base != raw
}
