package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

func server(calls *[]string) Server {
	return Server{
		Name: "gpuworks", Version: "test", Instructions: "read-only",
		Tools: []Tool{{Name: "echo", Description: "echoes", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Call: func(_ context.Context, name string, args json.RawMessage) (string, bool) {
			*calls = append(*calls, name+" "+string(args))
			return "said " + string(args), string(args) == `{"fail":true}`
		},
	}
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out["jsonrpc"] != "2.0" {
		t.Fatalf("reply %s: %v", raw, err)
	}
	return out
}

func TestInitializeNegotiatesTheProtocolVersion(t *testing.T) {
	var calls []string
	s := server(&calls)
	for asked, want := range map[string]string{"2025-03-26": "2025-03-26", "2024-11-05": "2024-11-05", "1999-01-01": "2025-06-18", "": "2025-06-18"} {
		out := decode(t, s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+asked+`"}}`)))
		res := out["result"].(map[string]any)
		if res["protocolVersion"] != want || res["serverInfo"].(map[string]any)["name"] != "gpuworks" || res["instructions"] != "read-only" || res["capabilities"].(map[string]any)["tools"] == nil {
			t.Fatalf("asked %q: %v", asked, out)
		}
	}
}

func TestNotificationsAreNotAnswered(t *testing.T) {
	var calls []string
	if reply := server(&calls).Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); reply != nil {
		t.Fatalf("reply = %s", reply)
	}
}

func TestToolsAreListedAndCalled(t *testing.T) {
	var calls []string
	s := server(&calls)
	out := decode(t, s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":"l","method":"tools/list"}`)))
	tools := out["result"].(map[string]any)["tools"].([]any)
	tool := tools[0].(map[string]any)
	if len(tools) != 1 || tool["name"] != "echo" || tool["inputSchema"].(map[string]any)["type"] != "object" || tool["annotations"].(map[string]any)["readOnlyHint"] != true || out["id"] != "l" {
		t.Fatalf("tools/list = %v", out)
	}
	for args, isError := range map[string]bool{`{"x":1}`: false, `{"fail":true}`: true} {
		out := decode(t, s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":`+args+`}}`)))
		res := out["result"].(map[string]any)
		content := res["content"].([]any)[0].(map[string]any)
		if content["type"] != "text" || content["text"] != "said "+args || res["isError"] != isError {
			t.Fatalf("call %s = %v", args, out)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestMalformedMessagesGetJSONRPCErrors(t *testing.T) {
	var calls []string
	s := server(&calls)
	for body, code := range map[string]float64{
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`: -32602,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":"not an object"}`:                -32602,
		`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`:                                     -32601,
		`[{"jsonrpc":"2.0","id":6,"method":"ping"}]`:                                             -32600,
		`{"id":7,"method":"ping"}`:                                                               -32600,
		`{not json`:                                                                              -32700,
	} {
		out := decode(t, s.Handle(context.Background(), []byte(body)))
		e, _ := out["error"].(map[string]any)
		if e == nil || e["code"] != code || out["result"] != nil {
			t.Errorf("%s: %v", body, out)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a malformed message ran a tool: %v", calls)
	}
	if out := decode(t, s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":9,"method":"ping"}`))); out["error"] != nil || out["id"] != float64(9) {
		t.Fatalf("ping = %v", out)
	}
}
