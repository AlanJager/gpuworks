package templateexport

import (
	"context"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

func TestChecksTestsCheapOperationsAndListsTheRest(t *testing.T) {
	contract := inferenceapi.Contract{Operations: []inferenceapi.Operation{
		{Type: inferenceapi.OperationChat, Endpoint: "/v1/chat/completions"},
		{Type: inferenceapi.OperationEmbeddings, Endpoint: "/v1/embeddings"},
		{Type: "openai.video.generate", Endpoint: "/v1/videos"},
	}}
	var sent []string
	probe := func(ctx context.Context, id uint, path string, payload map[string]any) ProbeReply {
		sent = append(sent, path)
		if _, deadline := ctx.Deadline(); !deadline {
			t.Error("a check was sent without a deadline")
		}
		if path == "/v1/embeddings" {
			if payload["input"] == nil {
				t.Errorf("embedding payload = %v", payload)
			}
			return ProbeReply{HTTPStatus: 503, Message: "busy"}
		}
		return ProbeReply{HTTPStatus: 200}
	}
	checks := Checks(context.Background(), probe, 7, contract)
	if !slices.Equal(sent, []string{"/v1/chat/completions", "/v1/embeddings"}) {
		t.Fatalf("sent %q: a video request must not be sent", sent)
	}
	if !checks[0].Tested || !checks[0].Passed || checks[1].Passed || checks[1].Message != "busy" || checks[2].Tested {
		t.Fatalf("checks = %+v", checks)
	}
	noReplica := Checks(context.Background(), func(context.Context, uint, string, map[string]any) ProbeReply { return ProbeReply{NoReplica: true} }, 7, contract)
	if noReplica[0].Passed || noReplica[0].Message != "no running replica" {
		t.Fatalf("no replica = %+v", noReplica[0])
	}
}
