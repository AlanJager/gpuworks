package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestBenchmarkManagedTargetRequiresDeclaredChatBeforeCreatingRun(t *testing.T) {
	for _, operations := range []string{`[{"type":"openai.audio.transcribe"}]`, `[]`, `[{"type":"unknown"}]`, `[{"type":"openai.chat"}]`} {
		for _, entry := range []string{"gateway", "node", "pinned"} {
			t.Run(operations+"/"+entry, func(t *testing.T) {
				fixture := newBenchmarkAdmission(t, "benchmark_chat_admission_"+strings.NewReplacer(".", "_", "/", "_", "[", "", "]", "", "{", "", "}", "", `"`, "", ":", "_").Replace(operations)+entry)
				fixture.handlers.d.Server = StaticServerSettings{Loopback: "http://127.0.0.1:1"}
				dep, node := fixture.managedTarget(t, "target")
				// Update an existing deployment's current model contract: the admission
				// check must not trust an earlier deployment snapshot or ready instance.
				if err := fixture.store.DB().Model(&store.Model{}).Where("id = ?", dep.ModelID).Update("operations", operations).Error; err != nil {
					t.Fatal(err)
				}
				origin := node.UUID
				if entry == "gateway" {
					origin = "server"
				}
				body := conflictBody(dep.ID, origin, false)
				if entry == "pinned" {
					body = strings.TrimSuffix(body, "}") + `,"instance_uuid":"target-leader"}`
				}
				response := fixture.create(t, body)
				runs, err := fixture.store.ListBenchmarkRuns()
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(operations, "openai.chat") {
					if response.Code != http.StatusAccepted || len(runs) != 1 {
						t.Fatalf("chat rejected: %d %s runs=%d", response.Code, response.Body.String(), len(runs))
					}
				} else {
					var wire struct {
						Code string `json:"code"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
						t.Fatal(err)
					}
					if response.Code != http.StatusBadRequest || wire.Code != "BENCHMARK_CHAT_REQUIRED" || len(runs) != 0 {
						t.Fatalf("unsupported target reached benchmark execution: status=%d body=%s runs=%d", response.Code, response.Body.String(), len(runs))
					}
				}
			})
		}
	}
}

func benchmarkChatModel(t *testing.T, st *store.Store, name string) *store.Model {
	t.Helper()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/" + name, Revision: "main", Name: name, Operations: `[{"type":"openai.chat"}]`})
	if err != nil {
		t.Fatal(err)
	}
	return model
}
