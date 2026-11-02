package aihelper

import (
	"net/url"
	"strings"
)

// chatCompletionsPath is the call every OpenAI-compatible client appends to
// Endpoint.BaseURL; ChatCompletionsURL is how it is appended.
const chatCompletionsPath = "/chat/completions"

// NormalizeBaseURL answers the address an OpenAI-compatible client is given,
// not the call it makes. Operators paste whatever their provider's
// documentation shows, and showing the whole call path is common - a client
// that then appends "/chat/completions" asks
// ".../v1/chat/completions/chat/completions" and gets 404. The call path is
// dropped and trailing slashes trimmed, so ".../v1/chat/completions" becomes
// ".../v1". A bare host (some providers serve the path from the root) is left
// alone. The bool reports a change, so a form can say what was saved.
//
// The address is taken apart rather than matched as text, because a base
// address may carry a query of its own: Azure serves
// ".../openai/deployments/<name>?api-version=<date>", and there the
// ?api-version= is part of the address, not part of the call. So only the path
// is rewritten and a query survives verbatim - dropping it would store a base
// that cannot be called at all, which is the defect this normalization exists
// to prevent, one shape along. A fragment survives for the same reason: it
// never reaches the server, and silently editing the operator's address to
// remove a harmless part of it is not this function's business. What the
// invariant does demand is that the stored base plus the call path is a sane
// call URL, and that is why the call is appended to the path
// (ChatCompletionsURL) instead of to the end of the string: "?x=1" then
// belongs to ".../chat/completions?x=1", never to "...?x=1/chat/completions".
//
// Stripping repeats, so normalizing an already-normalized address changes
// nothing (idempotent), and an address pasted twice loses both copies of the
// call path rather than one.
//
// A string net/url cannot parse is trimmed as text and returned unchanged
// otherwise: address validation rejects it right after, and this function does
// not try to repair an address it cannot read.
func NormalizeBaseURL(raw string) (string, bool) {
	base := strings.TrimSpace(raw)
	u, err := url.Parse(base)
	if err != nil {
		out := strings.TrimRight(base, "/")
		return out, out != raw
	}
	path := trimCallPath(u.Path)
	if path == u.Path {
		return base, base != raw
	}
	u.Path, u.RawPath = path, ""
	out := u.String()
	return out, out != raw
}

// trimCallPath drops the pasted call path from a URL path, together with the
// trailing slashes around it. It repeats so that an address pasted twice loses
// both copies: one strip would leave ".../chat/completions" behind, which
// normalizing again would strip, and normalizing twice must not differ from
// normalizing once.
func trimCallPath(path string) string {
	for {
		trimmed := strings.TrimRight(path, "/")
		// The leading slash of the constant is the segment boundary: a tail
		// such as "xchat/completions" or "v1beta" is a path of its own, not
		// the call.
		if len(trimmed) < len(chatCompletionsPath) || !strings.EqualFold(trimmed[len(trimmed)-len(chatCompletionsPath):], chatCompletionsPath) {
			return trimmed
		}
		path = trimmed[:len(trimmed)-len(chatCompletionsPath)]
	}
}

// ChatCompletionsURL is the URL a client posts to for a base address: the call
// appended to the address's path, not to the end of the string. A base that
// carries a query - Azure's ?api-version= - then sends that query with the
// call, and a base stored before normalization existed (a pasted call path)
// still works, because the address is normalized here too.
func ChatCompletionsURL(base string) (string, error) {
	normalized, _ := NormalizeBaseURL(base)
	u, err := url.Parse(normalized)
	if err != nil {
		return "", err
	}
	u.Path, u.RawPath = strings.TrimRight(u.Path, "/")+chatCompletionsPath, ""
	return u.String(), nil
}
