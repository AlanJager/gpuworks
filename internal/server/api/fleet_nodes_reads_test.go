package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// Explicit node inventory/facet requests refresh the shared snapshot, so
// external commits are visible. Dashboard/storage readers then reuse it;
// background reads retain the original bounded-report cache behavior.
func TestNodePagesShareTheFleetSnapshot(t *testing.T) {
	r := newAutoRig(t, 3)
	r.st.EnableFleetNodeSnapshot(func() time.Time { return time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC) })
	reads := countWholeNodeReads(t, r.st)
	paths := []string{"/api/v1/nodes?page=1&page_size=20", "/api/v1/nodes/facets", "/api/v1/dashboard/summary", "/api/v1/storages/usage"}
	get := func(path string) []byte {
		t.Helper()
		rec := r.do(r.session, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	for _, path := range paths {
		get(path)
	}
	reads.Store(0)
	for range 3 {
		for i, path := range paths {
			before := reads.Load()
			get(path)
			want := int64(0)
			if i < 2 {
				want = 1
			}
			if delta := reads.Load() - before; delta != want {
				t.Fatalf("GET %s read fleet %d times, want %d", path, delta, want)
			}
		}
	}
	reads.Store(0)

	if rec := r.do(r.session, http.MethodPatch, "/api/v1/nodes/au-2/name", `{"name":"renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body)
	}
	var page dto.NodePage
	if err := json.Unmarshal(get("/api/v1/nodes?page=1&page_size=20"), &page); err != nil {
		t.Fatal(err)
	}
	for _, n := range page.Items {
		if n.UUID == "au-2" && n.Name != "renamed" {
			t.Fatalf("the page after a rename shows %q", n.Name)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the page after a rename read the fleet %d times, want 1", n)
	}
}
