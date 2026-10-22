package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
)

func (r *tokenRig) rpc(bearer, body string) map[string]any {
	r.t.Helper()
	rec := r.do(bearer, "POST", "/api/v1/mcp", body)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		r.t.Fatalf("%s: %d %q %s", body, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["jsonrpc"] != "2.0" {
		r.t.Fatalf("%s: %s", body, rec.Body)
	}
	return out
}

func TestMCPIsOffUntilAnAdministratorTurnsItOn(t *testing.T) {
	rig := newTokenRig(t)
	read := rig.mint("read")
	rec := rig.do(read, "POST", "/api/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != `{"code":"MCP_DISABLED","error":"the MCP service is turned off"}` {
		t.Fatalf("off: %d %s", rec.Code, rec.Body)
	}
	status := rig.do(read, "GET", "/api/v1/mcp/status", "")
	var st map[string]any
	_ = json.Unmarshal(status.Body.Bytes(), &st)
	tools, _ := st["tools"].([]any)
	if st["enabled"] != false || st["path"] != "/api/v1/mcp" || st["protocol_version"] != "2025-06-18" || len(tools) != len(aihelper.BuiltinTools()) || len(st) != 4 {
		t.Fatalf("status = %s", status.Body)
	}
	if rec := rig.do(rig.session, "PUT", "/api/v1/mcp/settings", `{"enabled":true}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("turn on: %d %s", rec.Code, rec.Body)
	}
	if out := rig.rpc(read, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); out["error"] != nil {
		t.Fatalf("on: %v", out)
	}
	if rec := rig.do(rig.session, "PUT", "/api/v1/mcp/settings", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("turn off: %d", rec.Code)
	}
	if rec := rig.do(read, "POST", "/api/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("off again: %d", rec.Code)
	}
	if rec := rig.do("", "POST", "/api/v1/mcp", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
}

// The protocol as an MCP client speaks it: initialize, the initialized
// notification, tools/list and tools/call, with a read token.
func TestMCPServesTheAssistantToolsToAReadToken(t *testing.T) {
	rig := newTokenRig(t)
	rig.do(rig.session, "PUT", "/api/v1/mcp/settings", `{"enabled":true}`)
	read := rig.mint("read")

	init := rig.rpc(read, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	result, _ := init["result"].(map[string]any)
	if init["id"] != float64(1) || result["protocolVersion"] != "2025-03-26" || result["capabilities"].(map[string]any)["tools"] == nil || result["serverInfo"].(map[string]any)["name"] != "gpuworks" {
		t.Fatalf("initialize = %v", init)
	}
	if out := rig.rpc(read, `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`); out["result"].(map[string]any)["protocolVersion"] != "2025-06-18" || out["id"] != "a" {
		t.Fatalf("unknown version = %v", out)
	}
	if rec := rig.do(read, "POST", "/api/v1/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("notification: %d %q", rec.Code, rec.Body)
	}

	list := rig.rpc(read, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var names []string
	for _, tool := range list["result"].(map[string]any)["tools"].([]any) {
		tm := tool.(map[string]any)
		if tm["inputSchema"] == nil || tm["description"] == "" || tm["annotations"].(map[string]any)["readOnlyHint"] != true {
			t.Fatalf("tool = %v", tm)
		}
		names = append(names, tm["name"].(string))
	}
	for _, want := range aihelper.BuiltinTools() {
		if !slices.Contains(names, want.Name) {
			t.Fatalf("tools/list = %v, missing %s", names, want.Name)
		}
	}

	call := func(name, args string) (string, bool) {
		out := rig.rpc(read, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
		res, _ := out["result"].(map[string]any)
		if res == nil {
			t.Fatalf("%s: %v", name, out)
		}
		content := res["content"].([]any)[0].(map[string]any)
		return content["text"].(string), res["isError"].(bool)
	}
	if text, isErr := call("search_api", `{"query":"mcp status"}`); isErr || !strings.Contains(text, "/api/v1/mcp/status") {
		t.Fatalf("search_api: %v %s", isErr, text)
	}
	// call_api reads as the token's user, through the token itself.
	if text, isErr := call("call_api", `{"path":"/api/v1/mcp/status","jq":".enabled"}`); isErr || text != "true" {
		t.Fatalf("call_api: %v %s", isErr, text)
	}
	if text, isErr := call("call_api", `{"path":"/api/v1/apikeys"}`); !isErr || !strings.Contains(text, "not readable") {
		t.Fatalf("denied route: %v %s", isErr, text)
	}
	if text, isErr := call("call_api", `{"path":"/api/v1/platform-tokens"}`); !isErr || !strings.Contains(text, "not readable") {
		t.Fatalf("token route: %v %s", isErr, text)
	}

	// The protocol's own errors are the mcp package's tests; here, that the
	// endpoint answers them as JSON-RPC rather than as HTTP errors.
	if out := rig.rpc(read, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`); out["error"].(map[string]any)["code"] != float64(-32602) {
		t.Fatalf("unknown tool: %v", out)
	}
	if rec := rig.do(read, "GET", "/api/v1/mcp", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
}
