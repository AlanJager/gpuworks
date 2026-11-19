package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An automatic draft whose manual script states fewer cards than its
// placement is a wrong request, not a pool with no room: on an idle node the
// evaluation names the launch error on the request (FIELD_INVALID with its
// text) and the create answers 400 with the same text. Neither says "no node
// satisfies the automatic placement", which would tell the operator to wait
// (GWKS-415).
func TestEvaluateAndCreateNameAnAutomaticDraftsOwnLaunchError(t *testing.T) {
	h, st, modelID := twoCardFixture(t)
	h.d.Online = func(string) bool { return true }
	const wantErr = "resolve launch: vllm topology: tensor parallel 1 × pipeline parallel 1 requires 1 GPUs, but placement assigned 2"
	body := fmt.Sprintf(`{"name":"auto-script","model_id":%d,"engine":"vllm","image":"example/engine:test","placement_mode":"auto","target_vendor":"nvidia","replicas":1,"nnodes":1,"placement":{"tp":2},"quick_params":{},"container_overrides":{"command_mode":"manual","cmd_display":%q}}`, modelID, vllmOneCardScript)

	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body.String())
	}
	var evaluate map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &evaluate); err != nil {
		t.Fatal(err)
	}
	wantJSON(t, string(evaluate["ok"]), `false`)
	wantJSON(t, string(evaluate["errors"]), fmt.Sprintf(`[{"field":"","code":"FIELD_INVALID","params":{"detail":%q}}]`, wantErr))
	if _, selected := evaluate["selected_node"]; selected {
		t.Fatalf("the preview selected a node: %s", evaluate["selected_node"])
	}

	status, created := createOnTwoCards(t, h, body)
	if status != http.StatusBadRequest {
		t.Fatalf("create = %d %s", status, created)
	}
	wantJSON(t, created, fmt.Sprintf(`{"code":"BAD_REQUEST","error":%q}`, wantErr))
	if rows, err := st.ListDeployments(); err != nil || len(rows) != 0 {
		t.Fatalf("a refused create stored %d deployments (%v)", len(rows), err)
	}
}
