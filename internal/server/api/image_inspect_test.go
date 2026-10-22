package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestInspectImageOnNodeErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inspect func(context.Context, uint, string) (proto.ImageInspectResult, error)
		status  int
		message string
	}{
		{"unconfigured", nil, http.StatusServiceUnavailable, "image inspection is not configured"},
		{"offline", func(context.Context, uint, string) (proto.ImageInspectResult, error) {
			return proto.ImageInspectResult{}, errors.New("target node is offline")
		}, http.StatusConflict, "target node is offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := apiHandlers{d: Deps{InspectImage: tc.inspect}}
			resp := postJSON(t, h.inspectImageOnNode, "/api/v1/images/42/nodes/node-1/inspect", "", map[string]string{"id": "42", "uuid": "node-1"})
			var got map[string]any
			if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if resp.Code != tc.status || got["error"] != tc.message {
				t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}
