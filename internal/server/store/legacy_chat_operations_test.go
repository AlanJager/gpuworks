package store

import "testing"

// The placeholder is judged by shape, so both spellings that reached model
// rows before templates declared their operations are recognized, and nothing
// a normalizing writer produces is.
func TestIsDefaultChatOperations(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want bool
	}{
		"the bare value migration 80 wrote":           {`[{"type":"openai.chat"}]`, true},
		"the same value with a null inputs key":       {`[{"inputs":null,"type":"openai.chat"}]`, true},
		"key order does not matter":                   {`[{"type":"openai.chat","inputs":null}]`, true},
		"whitespace does not matter":                  {"[ { \"type\" : \"openai.chat\" } ]", true},
		"a normalized chat contract is a real choice": {`[{"inputs":[],"type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`, false},
		"an endpoint alone makes it a real choice":    {`[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`, false},
		"declared inputs make it a real choice":       {`[{"inputs":[{"name":"prompt"}],"type":"openai.chat"}]`, false},
		"another operation is not the placeholder":    {`[{"type":"openai.embeddings"}]`, false},
		"two operations are not the placeholder":      {`[{"type":"openai.chat"},{"type":"openai.embeddings"}]`, false},
		"an empty list is not the legacy placeholder": {`[]`, false},
		"no operations at all":                        {``, false},
		"a non-string type":                           {`[{"type":1}]`, false},
		"an object rather than a list":                {`{"type":"openai.chat"}`, false},
		"text that is not JSON":                       {`openai.chat`, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsDefaultChatOperations(tc.raw); got != tc.want {
				t.Fatalf("IsDefaultChatOperations(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
