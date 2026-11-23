package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	wireResult := `{"repo_tags":["test/runner:v1","test/runner:latest"]}`
	deps := Deps{Store: st, Bus: st.Bus, AuthSigner: signer, Online: func(string) bool { return online }}
	var audit bytes.Buffer
	deps.Log = slog.New(slog.NewJSONHandler(&audit, nil))
	deps.CallNode = func(ctx context.Context, id, method string, in any) (json.RawMessage, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= proto.ImageArchivePreviewTimeout {
			t.Fatal("server must wait for the worker scan deadline")
		}
		calls++
		wire, err := json.Marshal(in)
		if err != nil || id != node.UUID || method != "image.preview_path" || string(wire) != `{"archive_path":"`+path+`"}` {
			t.Fatalf("unexpected preview RPC %s %s %s %v", id, method, wire, err)
		}
		return json.RawMessage(wireResult), rpcErr
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
	var event map[string]any
	if err := json.Unmarshal(audit.Bytes(), &event); err != nil || event["username"] != "admin" || event["archive_path"] != path || event["node_uuid"] != node.UUID || event["action"] != "preview" {
		t.Fatalf("audit = %s", audit.Bytes())
	}
	wireResult = `{"repo_tags":["test/runner:v1"],"metadata_by_tag":{"test/runner:v1":{"engine":"vllm","engine_version":"0.30.0","framework":"cuda","framework_version":"13.0","arch":"amd64"}}}`
	if out := request("POST", route, admin, body, http.StatusOK); out != wireResult {
		t.Fatalf("preview metadata response: %s", out)
	}
	wireResult = `{"repo_tags":["test/runner:v1","test/other:v1"],"metadata_by_tag":{"test/runner:v1":{"engine":"sglang-npu","engine_version":"1.2"},"test/other:v1":{"engine":"unknown-engine","arch":"amd64"}}}`
	if out := request("POST", route, admin, body, http.StatusOK); out != `{"repo_tags":["test/runner:v1","test/other:v1"],"metadata_by_tag":{"test/other:v1":{"arch":"amd64"},"test/runner:v1":{"engine":"sglang","engine_version":"1.2"}}}` {
		t.Fatalf("node preview returned invalid catalog values: %s", out)
	}
	rpcErr = proto.ErrRPCUnknownMethod
	if out := request("POST", route, admin, body, http.StatusConflict); !strings.Contains(out, "AGENT_UPGRADE_REQUIRED") {
		t.Fatalf("old agent response: %s", out)
	}
	rpcErr = &proto.RPCError{Code: proto.ErrorArchivePreview, Message: "raw filesystem detail", Params: map[string]any{"reason": proto.ArchivePreviewNotFound}}
	if out := request("POST", route, admin, body, http.StatusConflict); !strings.Contains(out, `"reason":"not_found"`) || strings.Contains(out, "raw filesystem detail") {
		t.Fatalf("coded preview refusal: %s", out)
	}
	rpcErr = context.DeadlineExceeded
	if out := request("POST", route, admin, body, http.StatusConflict); !strings.Contains(out, `"reason":"timeout"`) {
		t.Fatalf("server deadline: %s", out)
	}
	rpcErr = errors.New("file missing")
	if out := request("POST", route, admin, body, http.StatusConflict); !strings.Contains(out, "IMAGE_ARCHIVE_PREVIEW_FAILED") {
		t.Fatalf("failed preview: %s", out)
	}
}
