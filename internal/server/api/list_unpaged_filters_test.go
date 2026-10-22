package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// A filter is never dropped: without page parameters a list answers every
// match in full, the same rows in the same order as one page holding them
// all. (On gpu3, GET /models?q=… without page returned every model, and the
// assistant concluded the filter was broken.)

func TestModelListWithoutPageAppliesFiltersAndOrder(t *testing.T) {
	f := newModelListFixture(t)
	for _, query := range []string{
		"q=baai", "source=modelscope,local&sort=name", "operation=openai.chat,openai.embeddings&sort=-weight",
		"source_id=qwen/QWEN3-8B", "source_id=org/nothing", "sort=published_at", "",
	} {
		rec := httptest.NewRecorder()
		f.handler.listModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models?"+query, nil))
		var full []dto.Model
		if err := unmarshalList(rec.Body.Bytes(), &full); err != nil {
			t.Fatalf("%s: %v %s", query, err, rec.Body.String())
		}
		got := make([]string, 0, len(full))
		for _, m := range full {
			got = append(got, m.Name)
		}
		paged := "page=1&page_size=200"
		if query != "" {
			paged += "&" + query
		}
		if want := modelNames(f.page(t, paged)); !reflect.DeepEqual(got, want) {
			t.Errorf("%q unpaged = %v, paged = %v", query, got, want)
		}
	}
	// Unpaged rows stay whole: the presence list is not cut to a summary.
	rec := httptest.NewRecorder()
	f.handler.listModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models?q=qwen", nil))
	var full []dto.Model
	if err := unmarshalList(rec.Body.Bytes(), &full); err != nil || len(full) != 1 || len(full[0].Nodes) == 0 {
		t.Fatalf("q=qwen unpaged = %s", rec.Body.String())
	}
}

func TestDeploymentListWithoutPageAppliesFiltersAndOrder(t *testing.T) {
	f := newListFixture(t, 12)
	for _, query := range []string{"q=EXTRA-1&sort=name", "q=node-gone", "q=nothing-like-it", "state=running&sort=-name", "sort=replicas"} {
		var full []dto.Deployment
		if err := unmarshalList(f.get(t, f.handler.listDeployments, "/api/v1/deployments?"+query), &full); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		got := make([]string, 0, len(full))
		for _, d := range full {
			got = append(got, d.Name)
		}
		want := pageNames(decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&page_size=200&"+query)))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q unpaged = %v, paged = %v", query, got, want)
		}
	}
}

func TestNodeListWithoutPageAppliesFiltersAndOrder(t *testing.T) {
	f := newListFixture(t, 0)
	read := func(path string) []string {
		var out []string
		body := f.get(t, f.handler.listNodes, path)
		var page dto.NodePage
		if json.Unmarshal(body, &page) == nil && page.PageSize > 0 {
			for _, n := range page.Items {
				out = append(out, n.Hostname)
			}
			return out
		}
		var full []dto.Node
		if err := unmarshalList(body, &full); err != nil {
			t.Fatalf("%s: %v %s", path, err, body)
		}
		for _, n := range full {
			out = append(out, n.Hostname)
		}
		return out
	}
	for _, query := range []string{"sort=-name", "q=list-b", "q=nothing", "state=online&sort=name"} {
		got := read("/api/v1/nodes?" + query)
		if want := read("/api/v1/nodes?page=1&page_size=200&" + query); !reflect.DeepEqual(got, want) {
			t.Errorf("%q unpaged = %v, paged = %v", query, got, want)
		}
	}
	if got := read("/api/v1/nodes?q=list-b"); !reflect.DeepEqual(got, []string{"list-b"}) {
		t.Fatalf("q=list-b unpaged = %v", got)
	}
}
