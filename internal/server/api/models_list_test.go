package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type modelListFixture struct {
	st      *store.Store
	handler *apiHandlers
}

// newModelListFixture seeds three models on two nodes: one ready on both and
// in a transfer nowhere, one downloading on one node and failed on the other,
// and one nowhere at all.
func newModelListFixture(t *testing.T) modelListFixture {
	t.Helper()
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err := st.RegisterNode("model-a", store.HashToken("model-a"), "model-a")
	must(err)
	b, err := st.RegisterNode("model-b", store.HashToken("model-b"), "model-b")
	must(err)
	published := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	qwen, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main", Name: "Qwen3-8B",
		README: "# readme", WeightBytes: 16 << 30, OriginCreatedAt: &published,
		Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`})
	must(err)
	bge, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "BAAI/bge-m3", Revision: "master", Name: "bge-m3",
		WeightBytes: 2 << 30, OriginUpdatedAt: &updated,
		Operations: `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]`})
	must(err)
	_, err = st.UpsertModel(&store.Model{Source: "local", SourceID: "/models/tiny", Name: "tiny"})
	must(err)
	for _, mf := range []store.ModelFile{
		{ModelID: qwen.ID, NodeID: a.ID, State: proto.MFPresent, Path: "/m/q", SizeBytes: 100, Downloaded: 100},
		{ModelID: qwen.ID, NodeID: b.ID, State: proto.MFRegistered, Path: "/m/q", SizeBytes: 100, Downloaded: 100},
		{ModelID: bge.ID, NodeID: a.ID, State: proto.MFDownloading, Path: "/m/b", SizeBytes: 100, Downloaded: 40, RateBPS: 7},
		{ModelID: bge.ID, NodeID: b.ID, State: proto.MFFailed, Path: "/m/b", Message: "disk full"},
	} {
		_, err := st.UpsertModelFile(&mf)
		must(err)
	}
	return modelListFixture{st: st, handler: &apiHandlers{d: Deps{Store: st}}}
}

func (f modelListFixture) page(t *testing.T, query string) dto.ModelPage {
	t.Helper()
	rec := httptest.NewRecorder()
	f.handler.listModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models?"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /models?%s = %d %s", query, rec.Code, rec.Body.String())
	}
	var page dto.ModelPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func modelNames(page dto.ModelPage) []string {
	out := []string{}
	for _, m := range page.Items {
		out = append(out, m.Name)
	}
	return out
}

func TestModelPageSummarizesPresenceAndLeavesTheDetailOut(t *testing.T) {
	f := newModelListFixture(t)
	page := f.page(t, "page=1&sort=name")
	if !reflect.DeepEqual(modelNames(page), []string{"bge-m3", "Qwen3-8B", "tiny"}) || page.Total != 3 {
		t.Fatalf("rows = %v (total %d)", modelNames(page), page.Total)
	}
	want := map[string]dto.ModelPresenceSummary{
		"Qwen3-8B": {ReadyNodes: 2},
		"bge-m3":   {InFlight: 1, InFlightPercent: 40, Failed: 1},
		"tiny":     {},
	}
	for _, m := range page.Items {
		if m.Presence != want[m.Name] {
			t.Errorf("%s presence = %+v, want %+v", m.Name, m.Presence, want[m.Name])
		}
		if m.Nodes != nil || m.Storages != nil || m.README != "" {
			t.Errorf("%s row carries the detail: nodes=%v storages=%v readme=%q", m.Name, m.Nodes, m.Storages, m.README)
		}
	}
	wantSummary := dto.ModelListSummary{Registered: 3, ReadyFiles: 2, Sources: 3, WeightGiB: 18}
	if page.Summary != wantSummary {
		t.Fatalf("summary = %+v, want %+v", page.Summary, wantSummary)
	}
	// The summary describes every model, whatever the filters keep.
	if narrowed := f.page(t, "page=1&q=tiny"); narrowed.Summary != wantSummary || !reflect.DeepEqual(modelNames(narrowed), []string{"tiny"}) {
		t.Fatalf("q=tiny = %v summary %+v", modelNames(narrowed), narrowed.Summary)
	}
}

func TestModelPageFiltersAndSortsBeforePaging(t *testing.T) {
	f := newModelListFixture(t)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"page=1&source=modelscope,local&sort=name", []string{"bge-m3", "tiny"}},
		{"page=1&operation=openai.chat", []string{"Qwen3-8B"}},
		{"page=1&operation=OPENAI.CHAT", []string{"Qwen3-8B"}},
		{"page=1&operation=OpenAI.Chat,OPENAI.EMBEDDINGS&sort=-weight", []string{"Qwen3-8B", "bge-m3"}},
		{"page=1&operation=openai.chat,openai.embeddings&sort=-weight", []string{"Qwen3-8B", "bge-m3"}},
		{"page=1&q=baai", []string{"bge-m3"}},
		// Published falls back to the last update; a model with neither sorts first.
		{"page=1&sort=published_at", []string{"tiny", "Qwen3-8B", "bge-m3"}},
		// Ties (no ready node) keep the natural order, newest first.
		{"page=1&sort=-presence", []string{"Qwen3-8B", "tiny", "bge-m3"}},
		{"page=2&page_size=2&sort=name", []string{"tiny"}},
		// source_id matches whole ids, ignoring case: not a substring, as q is.
		{"page=1&source_id=qwen/QWEN3-8B", []string{"Qwen3-8B"}},
		{"page=1&source_id=Qwen/Qwen3,BAAI/bge-m3&sort=name", []string{"bge-m3"}},
		{"page=1&source_id=org/nothing", []string{}},
		{"page=1&source_id=BAAI/bge-m3&source=huggingface", []string{}},
	} {
		if got := modelNames(f.page(t, tc.query)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestModelListWithoutPageKeepsThePresenceList(t *testing.T) {
	f := newModelListFixture(t)
	rec := httptest.NewRecorder()
	f.handler.listModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
	var full []dto.Model
	if err := unmarshalList(rec.Body.Bytes(), &full); err != nil || len(full) != 3 {
		t.Fatalf("full list = %s", rec.Body.String())
	}
	nodes := 0
	for _, m := range full {
		nodes += len(m.Nodes)
	}
	if nodes != 4 {
		t.Fatalf("the full list carries %d node presences, want 4", nodes)
	}
}

func TestModelPageRejectsUnknownFilters(t *testing.T) {
	f := newModelListFixture(t)
	for _, query := range []string{"page=1&source_id=a,", "page=1&source=github", "page=1&operation=openai.chatt", "page=1&sort=arch", "page=1&page_size=500"} {
		rec := httptest.NewRecorder()
		f.handler.listModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models?"+query, nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"LIST_`) {
			t.Errorf("%s = %d %s, want 400 LIST_*", query, rec.Code, rec.Body.String())
		}
	}
}

func TestModelPageWireJSON(t *testing.T) {
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	page := dto.ModelPage{
		ListPage: dto.ListPage[dto.ModelListItem]{Page: 1, PageSize: 20, Total: 1, Items: []dto.ModelListItem{{
			Model:    dto.Model{ID: 3, Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main", Name: "Qwen3-8B", WeightGiB: 16, CreatedAt: created},
			Presence: dto.ModelPresenceSummary{ReadyNodes: 96, CachedNodes: 1, ReadyStorages: 1, InFlight: 2, InFlightPercent: 50, Failed: 1},
		}}},
		Summary: dto.ModelListSummary{Registered: 436, ReadyFiles: 243, Sources: 3, WeightGiB: 21640},
	}
	got, _ := json.Marshal(page)
	const want = `{"items":[{"id":3,"source":"huggingface","source_id":"Qwen/Qwen3-8B","revision":"main","name":"Qwen3-8B","arch":"","quant_label":"","weight_gib":16,"operations":null,"input_modalities":null,"created_at":"2026-10-01T00:00:00Z","presence":{"ready_nodes":96,"cached_nodes":1,"ready_storages":1,"in_flight":2,"in_flight_percent":50,"failed":1}}],"page":1,"page_size":20,"total":1,"summary":{"registered":436,"ready_files":243,"sources":3,"weight_gib":21640}}`
	if string(got) != want {
		t.Fatalf("wire JSON\n got: %s\nwant: %s", got, want)
	}
	var back dto.ModelPage
	if err := json.Unmarshal(got, &back); err != nil || !reflect.DeepEqual(back, page) {
		t.Fatalf("round trip = %+v err %v", back, err)
	}
}

func TestModelOperationFilterMatchesCaseWithoutPaging(t *testing.T) {
	f := newModelListFixture(t)
	for _, query := range []string{"operation=OPENAI.CHAT", "operation=OpenAI.Chat,OPENAI.CHAT"} {
		rec := httptest.NewRecorder()
		f.handler.listModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models?"+query, nil))
		var rows []dto.Model
		if err := unmarshalList(rec.Body.Bytes(), &rows); rec.Code != http.StatusOK || err != nil || len(rows) != 1 || rows[0].SourceID != "Qwen/Qwen3-8B" {
			t.Fatalf("%s status=%d body=%s err=%v", query, rec.Code, rec.Body.String(), err)
		}
	}
}
