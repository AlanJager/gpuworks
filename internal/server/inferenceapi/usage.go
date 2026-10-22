package inferenceapi

// Usage is where an operation's response reports what it cost, so the gateway
// can count tokens and time the first one without knowing any protocol of its
// own (ADR 0039). Only engines report usage; nothing here is estimated.
type Usage struct {
	// Prompt and Completion are keys of the response's "usage" object.
	// Completion is empty for an operation that generates no tokens.
	Prompt     string
	Completion string
	// Cached is the key path, inside "usage", of the prompt tokens the engine
	// served from its cache. Empty when the protocol has no such field.
	Cached []string
	// Reasoning are key paths, inside "usage", where engines put the
	// completion tokens spent reasoning; the first present wins (vLLM and
	// TensorFold nest it, SGLang does not).
	Reasoning [][]string
	// StreamText are the keys of a streamed choice's delta whose first
	// non-empty value is the first generated token. Empty for an operation
	// that does not stream text.
	StreamText []string
	// StreamTextInChoice says the StreamText keys are members of the choice
	// itself (text completion: choices[0].text) rather than of its delta.
	StreamTextInChoice bool
}

var usages = map[string]Usage{
	OperationChat: {
		Prompt: "prompt_tokens", Completion: "completion_tokens",
		Cached:    []string{"prompt_tokens_details", "cached_tokens"},
		Reasoning: [][]string{{"completion_tokens_details", "reasoning_tokens"}, {"reasoning_tokens"}},
		// reasoning_content (vLLM, SGLang, TensorFold) and reasoning (newer
		// vLLM, OpenRouter) carry a reasoning model's first tokens; a reply
		// that opens with a tool call has no content at all.
		StreamText: []string{"content", "reasoning_content", "reasoning", "tool_calls"},
	},
	// Text completion reports usage as chat does; a streamed choice carries
	// its text directly, with no delta.
	OperationCompletions: {
		Prompt: "prompt_tokens", Completion: "completion_tokens",
		Cached:     []string{"prompt_tokens_details", "cached_tokens"},
		StreamText: []string{"text"}, StreamTextInChoice: true,
	},
	OperationEmbeddings: {Prompt: "prompt_tokens"},
}

// usageByEndpoint maps every endpoint an operation with usage may be served
// on to that usage. A deployment's endpoints are confined to its operations'
// allowed endpoints (NormalizeContract), so the request path alone says which
// protocol the response speaks.
var usageByEndpoint = func() map[string]Usage {
	index := map[string]Usage{}
	for operation, usage := range usages {
		for _, endpoint := range definitions[operation].AllowedEndpoints {
			index[endpoint] = usage
		}
	}
	return index
}()

// UsageForPath returns the usage of the operation served at a gateway request
// path (/v1/chat/completions), or false when the operation reports none.
func UsageForPath(path string) (Usage, bool) {
	usage, ok := usageByEndpoint[path]
	return usage, ok
}
