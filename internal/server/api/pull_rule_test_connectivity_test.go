package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSourceRuleTestAsksEachNodeAboutTheDraftsAddresses(t *testing.T) {
	st, err := store.Open("file:api_source_rule_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, uuid := range []string{"idc", "abroad", "offline", "old"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	var asked []proto.SourceProbeParams
	h := &apiHandlers{d: Deps{Store: st,
		Online: func(uuid string) bool { return uuid != "offline" },
		CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
			if method != proto.RPCSourceProbe {
				t.Errorf("rpc %s", method)
			}
			if uuid == "old" {
				return nil, errors.New("unknown rpc method source.probe")
			}
			p := params.(proto.SourceProbeParams)
			asked = append(asked, p)
			outcomes := []proto.SourceProbeOutcome{}
			for _, base := range p.Bases {
				status := proto.SourceProbeOK
				if uuid == "idc" && strings.Contains(base, "huggingface.co") {
					status = proto.SourceProbeUnreachable
				}
				outcomes = append(outcomes, proto.SourceProbeOutcome{Base: base, Status: status, LatencyMS: 12})
			}
			raw, _ := json.Marshal(proto.SourceProbeResult{Outcomes: outcomes})
			return raw, nil
		}}}
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.testSourceRule(rec, httptest.NewRequest(http.MethodPost, "/api/v1/pull/source-rules/test", strings.NewReader(body)))
		return rec
	}

	rec := post(`{"match":"https://HuggingFace.co/","endpoints":["https://hf-mirror.com/"],"allow_upstream":true,"node_uuid":"idc"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"source": "huggingface", "repo": "Qwen/Qwen3-0.6B", "node_uuid": "idc", "node_name": "idc",
		"outcomes": []any{
			map[string]any{"base": "https://hf-mirror.com", "status": "ok", "latency_ms": float64(12)},
			map[string]any{"base": "https://huggingface.co", "status": "unreachable", "latency_ms": float64(12)},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %s\nwant %v", rec.Body.String(), want)
	}
	if want := []string{"https://hf-mirror.com", "https://huggingface.co"}; len(asked) != 1 || !reflect.DeepEqual(asked[0].Bases, want) {
		t.Fatalf("asked = %+v, want bases %v", asked, want)
	}

	if rec := post(`{"match":"https://huggingface.co","endpoints":[],"allow_upstream":true,"node_uuid":"offline"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"NODE_OFFLINE"`) {
		t.Fatalf("offline = %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"match":"https://huggingface.co","endpoints":[],"allow_upstream":true,"node_uuid":"old"}`); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"code":"SOURCE_PROBE_FAILED"`) || !strings.Contains(rec.Body.String(), "unknown rpc method") {
		t.Fatalf("old agent = %d %s", rec.Code, rec.Body.String())
	}

	asked = nil
	if rec := post(`{"match":"https://modelscope.cn","endpoints":[],"allow_upstream":true,"node_uuid":"abroad","repo":"org/model"}`); rec.Code != 200 {
		t.Fatalf("modelscope = %d %s", rec.Code, rec.Body.String())
	}
	if len(asked) != 1 || asked[0].Source != "modelscope" || asked[0].Repo != "org/model" || !reflect.DeepEqual(asked[0].Bases, []string{"https://modelscope.cn"}) {
		t.Fatalf("modelscope asked = %+v", asked)
	}

	for body, field := range map[string]string{
		`{"match":"https://huggingface.co","endpoints":[],"allow_upstream":true}`:                                  "node_uuid",
		`{"match":"docker.io","endpoints":[],"allow_upstream":true,"node_uuid":"idc"}`:                             "match",
		`{"match":"https://huggingface.co/api","endpoints":[],"allow_upstream":true,"node_uuid":"idc"}`:            "match",
		`{"match":"https://huggingface.co","endpoints":["hf-mirror.com"],"allow_upstream":true,"node_uuid":"idc"}`: "endpoints",
		`{"match":"https://huggingface.co","endpoints":[],"allow_upstream":false,"node_uuid":"idc"}`:               "endpoints",
		`{"match":"https://huggingface.co","endpoints":[],"allow_upstream":true,"node_uuid":"idc","repo":"a b/c"}`: "repo",
	} {
		rec := post(body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"field":"`+field+`"`) {
			t.Errorf("%s: %d %s, want 400 on %s", body, rec.Code, rec.Body.String(), field)
		}
	}
	if rec := post(`{"match":"https://huggingface.co","endpoints":[],"allow_upstream":true,"node_uuids":["idc"]}`); rec.Code != 400 {
		t.Fatalf("the former node_uuids field = %d, want 400", rec.Code)
	}
	if rec := post(`{"match":"https://huggingface.co","endpoints":[],"allow_upstream":true,"node_uuid":"ghost"}`); rec.Code != 404 {
		t.Fatalf("unknown node = %d", rec.Code)
	}
}
