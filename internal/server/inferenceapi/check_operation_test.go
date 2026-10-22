package inferenceapi

import "testing"

func TestTheHealthCheckAsksChatFirstThenEmbeddings(t *testing.T) {
	for name, tc := range map[string]struct {
		ops  []Operation
		want string
	}{
		"chat":                 {[]Operation{{Type: OperationEmbeddings, Endpoint: "/v1/embeddings"}, {Type: OperationChat, Endpoint: "/v1/chat/completions"}}, OperationChat},
		"embeddings":           {[]Operation{{Type: OperationRerank, Endpoint: "/v1/rerank"}, {Type: OperationEmbeddings, Endpoint: "/v1/embeddings"}}, OperationEmbeddings},
		"neither":              {[]Operation{{Type: OperationRerank, Endpoint: "/v1/rerank"}}, ""},
		"no operations at all": {nil, ""},
	} {
		op, ok := Contract{Operations: tc.ops}.CheckOperation()
		if ok != (tc.want != "") || op.Type != tc.want {
			t.Errorf("%s: %+v %v", name, op, ok)
		}
	}
}
