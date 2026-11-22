package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestImagePreviewPathRoutesOnlyAuthorizedRequestsToTheSelectedNode(t *testing.T) {
	st, _, _ := newSessionRouter(t)
	node, _, err := st.CreateNodePending("archive-owner", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	const path = "/data/images/archive with spaces.tar"
	const body = `{"archive_path":"  /data/images/archive with spaces.tar  "}`
	route := "/api/v1/images/nodes/" + node.UUID + "/preview-archive-path"
	signer, admin := roleTestToken(t, st, "admin")
	_, viewer := roleTestToken(t, st, "viewer")
	calls := 0
	online := true
	var rpcErr error
	deps := Deps{Store: st, Bus: st.Bus, AuthSigner: signer, Online: func(string) bool { return online }}
	deps.CallNode = func(_ context.Context, id, method string, in any) (json.RawMessage, error) {
		calls++
		wire, err := json.Marshal(in)
		if err != nil || id != node.UUID || method != "image.preview_path" || string(wire) != `{"archive_path":"`+path+`"}` {
			t.Fatalf("unexpected preview RPC %s %s %s %v", id, method, wire, err)
		}
		return json.RawMessage(`{"repo_tags":["test/runner:v1","test/runner:latest"]}`), rpcErr
	}
	router := Router(deps)
	request := func(method, target, token, input string, status int) string {
		t.Helper()
		r := httptest.NewRequest(method, target, strings.NewReader(input))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s = %d, want %d: %s", method, target, w.Code, status, w.Body.String())
		}
		return strings.TrimSpace(w.Body.String())
	}
	request("POST", route, "", body, http.StatusUnauthorized)
	request("POST", route, viewer, body, http.StatusForbidden)
	request("GET", route, admin, "", http.StatusMethodNotAllowed)
	request("POST", route, admin, `{"archive_path":"file.tar"}`, http.StatusBadRequest)
	request("POST", "/api/v1/images/nodes/missing/preview-archive-path", admin, body, http.StatusNotFound)
	online = false
	request("POST", route, admin, body, http.StatusConflict)
	if calls != 0 {
		t.Fatalf("invalid requests reached the node %d times", calls)
	}
	online = true
	out := request("POST", route, admin, body, http.StatusOK)
	if out != `{"repo_tags":["test/runner:v1","test/runner:latest"]}` {
		t.Fatalf("preview response: %s", out)
	}
	rpcErr = proto.ErrRPCUnknownMethod
	if out := request("POST", route, admin, body, http.StatusConflict); !strings.Contains(out, "AGENT_UPGRADE_REQUIRED") {
		t.Fatalf("old agent response: %s", out)
	}
	rpcErr = errors.New("file missing")
	if out := request("POST", route, admin, body, http.StatusConflict); !strings.Contains(out, "IMAGE_ARCHIVE_PREVIEW_FAILED") {
		t.Fatalf("failed preview: %s", out)
	}
}
