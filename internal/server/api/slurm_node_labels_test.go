package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// A node that joined from a Slurm entry carries the entry (by UUID, shown by
// name) and its name in Slurm as system labels, on the list row and on the
// detail alike, as literal wire JSON; the node list filters by them and the
// facets offer them with the server's display. A node added by hand has none.
func TestSlurmNodesCarryTheirPlacementAsSystemLabels(t *testing.T) {
	rig := newTokenRig(t)
	uuid, tokens := enrolledSlurmNode(t, rig, "gpu-1", "gpu-2")
	_ = tokens
	var enrolled []string
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/nodes?source=cluster:"+uuid, "")
	var list struct {
		Items []dto.Node `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 2 {
		t.Fatalf("nodes of the entry = %d %s", rec.Code, rec.Body)
	}
	for _, n := range list.Items {
		enrolled = append(enrolled, n.UUID)
	}
	sort.Strings(enrolled)
	manual, _, err := rig.st.CreateNodePending("by-hand")
	if err != nil {
		t.Fatal(err)
	}

	wantCluster := fmt.Sprintf(`{"key":"gpuworks.ai/slurm-cluster","value":%q,"display":"hpc"}`, uuid)
	wantNode := `{"key":"gpuworks.ai/slurm-node","value":"gpu-1","display":"gpu-1"}`
	for _, path := range []string{"/api/v1/nodes?page=1&page_size=100", "/api/v1/nodes/" + enrolled[0], "/api/v1/nodes"} {
		rec := rig.do(rig.session, http.MethodGet, path, "")
		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(body, wantCluster) {
			t.Fatalf("GET %s = %d, lacks the entry label\n%s", path, rec.Code, body)
		}
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/nodes?page=1&page_size=100", ""); !strings.Contains(rec.Body.String(), wantNode) {
		t.Fatalf("the list lacks %s\n%s", wantNode, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/nodes/"+manual.UUID, ""); strings.Contains(rec.Body.String(), "slurm") {
		t.Fatalf("a hand-added node carries Slurm labels: %s", rec.Body)
	}

	listed := func(query string) []string {
		rec := rig.do(rig.session, http.MethodGet, "/api/v1/nodes?page=1&page_size=100&"+query, "")
		if rec.Code != 200 {
			t.Fatalf("%s = %d %s", query, rec.Code, rec.Body)
		}
		var page struct {
			Items []dto.Node `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, item := range page.Items {
			out = append(out, item.UUID)
		}
		sort.Strings(out)
		return out
	}
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"entry by UUID", labelQuery(nodelabel.KeySlurmCluster, uuid), enrolled},
		{"node name", labelQuery(nodelabel.KeySlurmNode, "gpu-1"), []string{nodeUUIDBySlurmName(t, list.Items, rig, "gpu-1")}},
		{"an unknown entry", labelQuery(nodelabel.KeySlurmCluster, "no-such-entry"), []string{}},
	} {
		if got := listed(tc.query); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %s matched %v, want %v", tc.name, tc.query, got, tc.want)
		}
	}

	rec = rig.do(rig.session, http.MethodGet, "/api/v1/nodes/facets", "")
	var facets dto.NodeFacets
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &facets) != nil {
		t.Fatalf("facets = %d %s", rec.Code, rec.Body)
	}
	system := map[string][]dto.SystemLabelValueCount{}
	for _, key := range facets.SystemLabels {
		system[key.Key] = key.Values
	}
	if got, want := system[nodelabel.KeySlurmCluster], []dto.SystemLabelValueCount{{Value: uuid, Display: "hpc", Nodes: 2}}; !reflect.DeepEqual(got, want) {
		t.Errorf("entry facets = %+v, want %+v", got, want)
	}
	if got, want := system[nodelabel.KeySlurmNode], []dto.SystemLabelValueCount{{Value: "gpu-1", Display: "gpu-1", Nodes: 1}, {Value: "gpu-2", Display: "gpu-2", Nodes: 1}}; !reflect.DeepEqual(got, want) {
		t.Errorf("node facets = %+v, want %+v", got, want)
	}
}

// nodeUUIDBySlurmName finds the enrolled node with the given Slurm name.
func nodeUUIDBySlurmName(t *testing.T, items []dto.Node, rig *tokenRig, name string) string {
	t.Helper()
	for _, n := range items {
		if n.SlurmNode == name {
			return n.UUID
		}
	}
	t.Fatalf("no enrolled node %s in %v", name, items)
	return ""
}
