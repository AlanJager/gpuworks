package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/slurmcluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// enrolledSlurmNode makes an entry through the API and enrolls nodes in it,
// returning the entry's UUID and each node's token.
func enrolledSlurmNode(t *testing.T, rig *tokenRig, names ...string) (uuid string, tokens map[string]string) {
	t.Helper()
	uuid, token, _ := createSlurm(t, rig, slurmCreateBody)
	tokens = map[string]string{}
	tok, _ := json.Marshal(token)
	for _, name := range names {
		rec := rig.do("", http.MethodPost, "/api/v1/slurm/enroll", `{"token":`+string(tok)+`,"slurm_cluster_name":"demo","node_name":"`+name+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("enroll %s = %d %s", name, rec.Code, rec.Body)
		}
		tokens[name] = decodeMap(t, rec.Body.Bytes())["node_token"].(string)
	}
	return uuid, tokens
}

func TestSlurmGPUNodeReportsReconcileWithTheEnrolledNodes(t *testing.T) {
	rig := newTokenRig(t)
	_, tokens := enrolledSlurmNode(t, rig, "gpu-1", "gpu-2")
	// gpu-1 is online, gpu-2 enrolled and offline; gpu-3 never enrolled.
	if err := rig.st.DB().Model(&store.Node{}).Where("slurm_node = ?", "gpu-1").Update("state", store.NodeStateOnline).Error; err != nil {
		t.Fatal(err)
	}
	if err := rig.st.DB().Model(&store.Node{}).Where("slurm_node = ?", "gpu-2").Update("state", store.NodeStateOffline).Error; err != nil {
		t.Fatal(err)
	}
	// The handler is called directly: the entry's list is written at most once a
	// minute, and the test moves the clock instead of waiting.
	h, clock := slurmReportHandler(rig)
	report := func(token, body string) (int, map[string]any) {
		rec := directSlurmReport(h, token, body)
		return rec.Code, decodeMap(t, rec.Body.Bytes())
	}
	settings := map[string]any{"sbatch_args": "-A ai --time=7-0", "idle_return_seconds": float64(60), "pending_timeout_seconds": float64(600), "account_user": ""}

	// A digest the server does not know asks for the nodes.
	code, out := report(tokens["gpu-1"], `{"digest":"d1"}`)
	if code != http.StatusOK || !reflect.DeepEqual(out, map[string]any{"need_nodes": true, "settings": settings}) {
		t.Fatalf("digest only = %d %v", code, out)
	}
	full := `{"digest":"d1","nodes":[{"name":"gpu-1","state":"IDLE","gpus":2},{"name":"gpu-2","state":"drained","reason":"gres/gpu count reported lower than configured (1 < 2)","gpus":2},` +
		`{"name":"gpu-3","state":"down","reason":"Not responding","gpus":4}]}`
	code, out = report(tokens["gpu-1"], full)
	if code != http.StatusOK || !reflect.DeepEqual(out, map[string]any{"need_nodes": false, "settings": settings}) {
		t.Fatalf("full report = %d %v", code, out)
	}
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "")
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list = %s", rec.Body)
	}
	item := list.Items[0]
	wantMissing := []any{
		map[string]any{"name": "gpu-2", "reason": "offline", "slurm_state": "drained", "slurm_reason": "gres/gpu count reported lower than configured (1 < 2)"},
		map[string]any{"name": "gpu-3", "reason": "not_enrolled", "slurm_state": "down", "slurm_reason": "Not responding"},
	}
	// The enrolled, offline node carries its UUID, for the console to offer
	// to remove it; one that never enrolled has none.
	missingNodes, _ := item["missing_nodes"].([]any)
	if len(missingNodes) == 2 {
		if first, _ := missingNodes[0].(map[string]any); first["node_uuid"] == "" || first["node_uuid"] == nil {
			t.Fatalf("the offline node has no node_uuid: %v", first)
		} else {
			delete(first, "node_uuid")
		}
	}
	// One of the two enrolled nodes is online: the list is current.
	_, asOf := item["list_as_of"]
	if item["node_count"] != float64(2) || item["online_count"] != float64(1) || asOf || item["slurm_gpu_nodes"] != float64(3) || item["missing_count"] != float64(2) || !reflect.DeepEqual(missingNodes, wantMissing) {
		t.Fatalf("reconciliation = %v", item)
	}

	// The same digest from another agent costs no write and no read of the
	// list: the rows keep their ids.
	var before []uint
	if err := rig.st.DB().Model(&store.SlurmGPUNode{}).Order("id").Pluck("id", &before).Error; err != nil || len(before) != 3 {
		t.Fatalf("rows = %v %v", before, err)
	}
	if code, out := report(tokens["gpu-2"], `{"digest":"d1"}`); code != http.StatusOK || out["need_nodes"] != false {
		t.Fatalf("repeated digest = %d %v", code, out)
	}
	// A full list of a digest the server already holds changes nothing either.
	if code, out := report(tokens["gpu-2"], full); code != http.StatusOK || out["need_nodes"] != false {
		t.Fatalf("full report of a known digest = %d %v", code, out)
	}
	var after []uint
	if err := rig.st.DB().Model(&store.SlurmGPUNode{}).Order("id").Pluck("id", &after).Error; err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rows were rewritten: %v -> %v (%v)", before, after, err)
	}

	// A changed list replaces the stored one (a minute after the last write).
	*clock = clock.Add(2 * time.Minute)
	changed := `{"digest":"d2","nodes":[{"name":"gpu-1","state":"idle","gpus":2}]}`
	// A new digest, as an agent sends it alone: with "nodes": null (this
	// version) the same as with no nodes key (earlier agents).
	if code, out := report(tokens["gpu-1"], `{"digest":"d2","state_digest":"","nodes":null}`); code != http.StatusOK || out["need_nodes"] != true {
		t.Fatalf("new digest, nodes null = %d %v", code, out)
	}
	if code, out := report(tokens["gpu-1"], `{"digest":"d2"}`); code != http.StatusOK || out["need_nodes"] != true {
		t.Fatalf("new digest = %d %v", code, out)
	}
	if code, _ := report(tokens["gpu-1"], changed); code != http.StatusOK {
		t.Fatal("changed report refused")
	}
	rec = rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	// gpu-2 enrolled long ago and is offline, and Slurm no longer lists it: it
	// is not a node Slurm has, so nothing is missing and it is not a GPU node
	// the entry has (a node enrolled within a report interval is, until a list
	// has had the chance to name it).
	if err := rig.st.DB().Model(&store.Node{}).Where("slurm_node = ?", "gpu-2").Update("created_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	rec = rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Items[0]["slurm_gpu_nodes"] != float64(1) || list.Items[0]["missing_count"] != float64(0) || !reflect.DeepEqual(list.Items[0]["missing_nodes"], []any{}) {
		t.Fatalf("after the change = %v", list.Items[0])
	}
}

// slurmReportHandler is the report handler with a clock the test moves.
func slurmReportHandler(rig *tokenRig) (*apiHandlers, *time.Time) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	h := &apiHandlers{d: Deps{Store: rig.st}, slurm: slurmcluster.New()}
	h.slurm.SetClock(func() time.Time { return now })
	return h, &now
}

func directSlurmReport(h *apiHandlers, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/slurm/gpu-nodes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.slurmReportGPUNodes(rec, req)
	return rec
}

func TestSlurmGPUNodeReportCarriesTheEntrysSettings(t *testing.T) {
	rig := newTokenRig(t)
	uuid, tokens := enrolledSlurmNode(t, rig, "gpu-1")
	rec := rig.do(rig.session, http.MethodPut, "/api/v1/slurm/clusters/"+uuid, `{"sbatch_args":"-p gpu","idle_return_seconds":120,"pending_timeout_seconds":900,"account_user":"svc"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body)
	}
	rec = rig.do(tokens["gpu-1"], http.MethodPost, "/api/v1/slurm/gpu-nodes", `{"digest":"x"}`)
	want := map[string]any{"need_nodes": true, "settings": map[string]any{"sbatch_args": "-p gpu", "idle_return_seconds": float64(120), "pending_timeout_seconds": float64(900), "account_user": "svc"}}
	if got := decodeMap(t, rec.Body.Bytes()); rec.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("report after the edit = %d %v", rec.Code, got)
	}
}

func TestSlurmGPUNodeReportValidation(t *testing.T) {
	rig := newTokenRig(t)
	_, tokens := enrolledSlurmNode(t, rig, "gpu-1")
	// Only an enrolled Slurm node reports: no token, a made-up one, an
	// administrator's session, and a node that joined any other way are refused.
	if _, err := rig.st.RegisterNode("uuid-hand", store.HashToken("hand-token"), "hand"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "nope", rig.session, "hand-token"} {
		if rec := rig.do(token, http.MethodPost, "/api/v1/slurm/gpu-nodes", `{"digest":"d"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %.8q = %d, want 401", token, rec.Code)
		}
	}
	good := tokens["gpu-1"]
	for name, body := range map[string]string{
		"no digest":      `{"nodes":[]}`,
		"empty digest":   `{"digest":" "}`,
		"long digest":    `{"digest":"` + strings.Repeat("a", 129) + `"}`,
		"not json":       `{`,
		"nameless node":  `{"digest":"d","nodes":[{"name":" ","state":"idle"}]}`,
		"long node name": `{"digest":"d","nodes":[{"name":"` + strings.Repeat("n", 65) + `","state":"idle"}]}`,
		"duplicate node": `{"digest":"d","nodes":[{"name":"a","state":"idle"},{"name":"a","state":"down"}]}`,
	} {
		if rec := rig.do(good, http.MethodPost, "/api/v1/slurm/gpu-nodes", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	// Slurm's own words are truncated at the columns, not refused; an explicit
	// empty list is a list (every node left the cluster).
	long := strings.Repeat("é", 200)
	h, clock := slurmReportHandler(rig)
	rec := directSlurmReport(h, good, `{"digest":"d1","nodes":[{"name":"a","state":"`+strings.Repeat("S", 40)+`","reason":"`+long+`","gpus":-3}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("long fields = %d %s", rec.Code, rec.Body)
	}
	var row store.SlurmGPUNode
	if err := rig.st.DB().First(&row).Error; err != nil || len(row.State) != 32 || len(row.Reason) > 256 || row.GPUs != 0 || !strings.HasPrefix(row.Reason, "éé") {
		t.Fatalf("row = %+v %v", row, err)
	}
	*clock = clock.Add(2 * time.Minute)
	if rec := directSlurmReport(h, good, `{"digest":"d0","nodes":[]}`); rec.Code != http.StatusOK || decodeMap(t, rec.Body.Bytes())["need_nodes"] != false {
		t.Fatalf("empty list = %d %s", rec.Code, rec.Body)
	}
	if n := countRows(t, rig, &store.SlurmGPUNode{}); n != 0 {
		t.Fatalf("an empty list left %d rows", n)
	}
	// A deleted entry's nodes cannot report any more.
	rig.st.DB().Model(&store.Node{}).Where("slurm_node = ?", "gpu-1").Update("slurm_cluster_id", 999)
	if rec := rig.do(good, http.MethodPost, "/api/v1/slurm/gpu-nodes", `{"digest":"d"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a node of a missing entry = %d", rec.Code)
	}
}

func countRows(t *testing.T, rig *tokenRig, model any) int64 {
	t.Helper()
	var n int64
	if err := rig.st.DB().Model(model).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSlurmClusterEdit(t *testing.T) {
	rig := newTokenRig(t)
	uuid, _, _ := createSlurm(t, rig, slurmCreateBody)
	rec := rig.do(rig.session, http.MethodPut, "/api/v1/slurm/clusters/"+uuid, `{"name":"renamed","account_user":"svc","sbatch_args":"","idle_return_seconds":300}`)
	got := decodeMap(t, rec.Body.Bytes())
	cluster, _ := got["cluster"].(map[string]any)
	if rec.Code != http.StatusOK || !reflect.DeepEqual(got["changed"], []any{"account_user", "idle_return_seconds", "name", "sbatch_args"}) ||
		cluster["name"] != "renamed" || cluster["account_user"] != "svc" || cluster["sbatch_args"] != "" || cluster["idle_return_seconds"] != float64(300) ||
		cluster["pending_timeout_seconds"] != float64(600) || cluster["setup_command"] == nil || cluster["server_url"] != "https://gw.example:8800" {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body)
	}
	// Nothing sent changes nothing.
	rec = rig.do(rig.session, http.MethodPut, "/api/v1/slurm/clusters/"+uuid, `{}`)
	if got := decodeMap(t, rec.Body.Bytes()); rec.Code != http.StatusOK || !reflect.DeepEqual(got["changed"], []any{}) {
		t.Fatalf("empty edit = %d %s", rec.Code, rec.Body)
	}
	// What the scripts on the nodes already name cannot be edited, and an
	// unknown field is refused rather than looking accepted.
	for _, body := range []string{`{"server_url":"https://other"}`, `{"run_as":"x"}`, `{"install_dir":"/x"}`, `{"script_dir":"/x"}`, `{"token":"x"}`, `{"nope":1}`, `{`} {
		if rec := rig.do(rig.session, http.MethodPut, "/api/v1/slurm/clusters/"+uuid, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", body, rec.Code, rec.Body)
		}
	}
	for _, tc := range []struct{ body, field string }{
		{`{"name":""}`, "name"}, {`{"account_user":"Bad User"}`, "account_user"}, {`{"idle_return_seconds":0}`, "idle_return_seconds"},
		{`{"pending_timeout_seconds":5}`, "pending_timeout_seconds"}, {`{"sbatch_args":"a\nb"}`, "sbatch_args"},
	} {
		rec := rig.do(rig.session, http.MethodPut, "/api/v1/slurm/clusters/"+uuid, tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorSlurmClusterInvalid) || !strings.Contains(rec.Body.String(), `"field":"`+tc.field+`"`) {
			t.Fatalf("%s = %d %s", tc.body, rec.Code, rec.Body)
		}
	}
	rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters", `{"name":"other","server_url":"https://gw"}`)
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/slurm/clusters/"+uuid, `{"name":"other"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorSlurmClusterNameTaken) {
		t.Fatalf("rename onto another entry = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/slurm/clusters/missing", `{"name":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("edit of a missing entry = %d", rec.Code)
	}
}

// State changes do not rewrite the list: with the membership unchanged the
// server stores nothing for a state digest it already has, asks for the list
// when the states changed, updates only the rows that differ, and does it at
// most once a minute per entry. Listed-only nodes are kept by name and no GPU
// node count or missing node comes of them.
func TestSlurmReportStatesChangeRowsInPlaceAndAtMostOncePerMinute(t *testing.T) {
	rig := newTokenRig(t)
	_, tokens := enrolledSlurmNode(t, rig, "gpu-1")
	h, clock := slurmReportHandler(rig)
	report := func(body string) map[string]any {
		t.Helper()
		rec := directSlurmReport(h, tokens["gpu-1"], body)
		if rec.Code != http.StatusOK {
			t.Fatalf("report = %d %s", rec.Code, rec.Body)
		}
		return decodeMap(t, rec.Body.Bytes())
	}
	rows := func() map[string]store.SlurmGPUNode {
		var all []store.SlurmGPUNode
		if err := rig.st.DB().Order("id").Find(&all).Error; err != nil {
			t.Fatal(err)
		}
		out := map[string]store.SlurmGPUNode{}
		for _, r := range all {
			out[r.Name] = r
		}
		return out
	}
	list := func(state string) string {
		return `"nodes":[{"name":"gpu-1","state":"` + state + `","gpus":2},{"name":"gpu-2","state":"idle","gpus":2},{"name":"cpu-1","listed_only":true,"state":"","gpus":0}]`
	}
	if got := report(`{"digest":"m1","state_digest":"s1"}`); got["need_nodes"] != true {
		t.Fatalf("first digests = %v", got)
	}
	report(`{"digest":"m1","state_digest":"s1",` + list("idle") + `}`)
	first := rows()
	if len(first) != 3 || !first["cpu-1"].ListedOnly || first["gpu-1"].ListedOnly {
		t.Fatalf("rows = %+v", first)
	}
	// The same digests: nothing to ask, nothing to write.
	if got := report(`{"digest":"m1","state_digest":"s1"}`); got["need_nodes"] != false {
		t.Fatalf("known digests = %v", got)
	}
	// A state changed within the minute: it waits, the server does not even ask.
	*clock = clock.Add(30 * time.Second)
	if got := report(`{"digest":"m1","state_digest":"s2"}`); got["need_nodes"] != false {
		t.Fatalf("a change within a minute of the last write was asked for: %v", got)
	}
	if got := report(`{"digest":"m1","state_digest":"s2",` + list("drained") + `}`); got["need_nodes"] != false || rows()["gpu-1"].State != "idle" {
		t.Fatalf("a list sent within the minute was stored: %+v", rows()["gpu-1"])
	}
	// A minute on, the next report that carries it is asked for the list, and the
	// row alone is updated.
	*clock = clock.Add(31 * time.Second)
	if got := report(`{"digest":"m1","state_digest":"s2"}`); got["need_nodes"] != true {
		t.Fatalf("a change after the minute = %v", got)
	}
	report(`{"digest":"m1","state_digest":"s2",` + list("drained") + `}`)
	after := rows()
	if after["gpu-1"].State != "drained" || after["gpu-1"].ID != first["gpu-1"].ID || after["gpu-2"].ID != first["gpu-2"].ID || after["cpu-1"].ID != first["cpu-1"].ID {
		t.Fatalf("rows after a state change = %+v (before %+v)", after, first)
	}
	// The same digests are free again, and a changed membership is stored (after
	// its own minute) by replacing the rows.
	if got := report(`{"digest":"m1","state_digest":"s2"}`); got["need_nodes"] != false {
		t.Fatalf("known digests after the update = %v", got)
	}
	*clock = clock.Add(2 * time.Minute)
	if got := report(`{"digest":"m2","state_digest":"s2"}`); got["need_nodes"] != true {
		t.Fatalf("a new membership = %v", got)
	}
	report(`{"digest":"m2","state_digest":"s2","nodes":[{"name":"gpu-1","state":"drained","gpus":2}]}`)
	if got := rows(); len(got) != 1 || got["gpu-1"].ID == first["gpu-1"].ID {
		t.Fatalf("rows after the membership changed = %+v", got)
	}
	// The entry shows one GPU node and no missing node from the listed-only one.
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "")
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Items) != 1 || out.Items[0]["slurm_gpu_nodes"] != float64(1) {
		t.Fatalf("list = %s (%v)", rec.Body, err)
	}
}

// The list the agents report is up to a report interval old, so a node that
// just enrolled may not be on it: the entry never shows fewer GPU nodes than
// nodes that joined in that lag. Once a report lists it the list's count is the
// one shown, and a node a list left out long after it enrolled is not counted.
func TestSlurmGPUNodeCountIncludesNodesThatEnrolledSinceTheLastList(t *testing.T) {
	rig := newTokenRig(t)
	uuid, tokens := enrolledSlurmNode(t, rig, "gpu-1")
	h, clock := slurmReportHandler(rig)
	send := func(body string) {
		t.Helper()
		*clock = clock.Add(2 * time.Minute)
		if rec := directSlurmReport(h, tokens["gpu-1"], body); rec.Code != http.StatusOK {
			t.Fatalf("report = %d %s", rec.Code, rec.Body)
		}
	}
	counts := func() (gpu, joined float64) {
		t.Helper()
		rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "")
		var list struct{ Items []map[string]any }
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
			t.Fatalf("list = %d %s", rec.Code, rec.Body)
		}
		return list.Items[0]["slurm_gpu_nodes"].(float64), list.Items[0]["node_count"].(float64)
	}
	send(`{"digest":"d1","state_digest":"s1","nodes":[{"name":"gpu-1","state":"idle","gpus":1}]}`)
	if gpu, joined := counts(); gpu != 1 || joined != 1 {
		t.Fatalf("one listed, one joined = %v, %v", gpu, joined)
	}
	// A second node enrolls (a scale-out) before any report lists it.
	tok, _ := json.Marshal(uuidToken(t, rig, uuid))
	if rec := rig.do("", http.MethodPost, "/api/v1/slurm/enroll", `{"token":`+string(tok)+`,"slurm_cluster_name":"demo","node_name":"gpu-2"}`); rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d %s", rec.Code, rec.Body)
	}
	if gpu, joined := counts(); gpu != 2 || joined != 2 {
		t.Fatalf("two joined, one listed = GPU nodes %v, joined %v", gpu, joined)
	}
	// Missing is the list's: gpu-2 is not on it, so it is not missing (gpu-1 is
	// listed and has no agent online).
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "")
	if strings.Contains(rec.Body.String(), `"name":"gpu-2","reason"`) || !strings.Contains(rec.Body.String(), `"missing_count":1`) {
		t.Fatalf("an unlisted joined node is counted missing: %s", rec.Body)
	}
	// The next report lists it: the count is the list's, not counted twice.
	send(`{"digest":"d2","state_digest":"s2","nodes":[{"name":"gpu-1","state":"idle","gpus":1},{"name":"gpu-2","state":"idle","gpus":1}]}`)
	if gpu, joined := counts(); gpu != 2 || joined != 2 {
		t.Fatalf("both listed = GPU nodes %v, joined %v", gpu, joined)
	}
	// Once the lag has passed, a list that leaves a node out means it has left.
	send(`{"digest":"d3","state_digest":"s3","nodes":[{"name":"gpu-1","state":"idle","gpus":1}]}`)
	if err := rig.st.DB().Model(&store.Node{}).Where("slurm_node = ?", "gpu-2").Update("created_at", time.Now().UTC().Add(-slurmcluster.ListLag-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if gpu, joined := counts(); gpu != 1 || joined != 2 {
		t.Fatalf("a node a later list leaves out = GPU nodes %v, joined %v", gpu, joined)
	}
}

// uuidToken is the enrollment token of the entry with this UUID.
func uuidToken(t *testing.T, rig *tokenRig, uuid string) string {
	t.Helper()
	c, err := rig.st.SlurmClusterByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return c.Token
}

// slurmEntryItem is the one entry of the list.
func slurmEntryItem(t *testing.T, rig *tokenRig) map[string]any {
	t.Helper()
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "")
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list = %s", rec.Body)
	}
	return list.Items[0]
}

// A node removed by hand (the console's Remove on an offline node) goes off
// its entry's list with it, so an entry whose last agent left stops counting
// it; a node still in the cluster is back on the list with the next report,
// even one with the digest the server remembers. With no agent online the
// list says how old it can be.
func TestRemovingASlurmNodeTakesItOffTheListUntilAReportBringsItBack(t *testing.T) {
	rig := newTokenRig(t)
	_, tokens := enrolledSlurmNode(t, rig, "gpu-1", "gpu-2")
	lastSeen := time.Date(2026, 11, 18, 11, 20, 0, 0, time.UTC)
	set := func(name, state string) {
		if err := rig.st.DB().Model(&store.Node{}).Where("slurm_node = ?", name).Updates(map[string]any{"state": state, "last_seen_at": lastSeen}).Error; err != nil {
			t.Fatal(err)
		}
	}
	set("gpu-1", store.NodeStateOnline)
	set("gpu-2", store.NodeStateOffline)
	full := `{"digest":"d1","nodes":[{"name":"gpu-1","state":"idle","gpus":1},{"name":"gpu-2","state":"down","gpus":1}]}`
	if rec := rig.do(tokens["gpu-1"], http.MethodPost, "/api/v1/slurm/gpu-nodes", full); rec.Code != http.StatusOK {
		t.Fatalf("report = %d %s", rec.Code, rec.Body)
	}
	item := slurmEntryItem(t, rig)
	missing, _ := item["missing_nodes"].([]any)
	if item["slurm_gpu_nodes"] != float64(2) || item["online_count"] != float64(1) || len(missing) != 1 {
		t.Fatalf("before = %v", item)
	}
	uuid, _ := missing[0].(map[string]any)["node_uuid"].(string)
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/nodes/"+uuid, ""); rec.Code != http.StatusOK {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	if item = slurmEntryItem(t, rig); item["slurm_gpu_nodes"] != float64(1) || item["missing_count"] != float64(0) || item["node_count"] != float64(1) {
		t.Fatalf("after the removal = %v", item)
	}
	// The same digest again: the server asks for the nodes, and stores them.
	rec := rig.do(tokens["gpu-1"], http.MethodPost, "/api/v1/slurm/gpu-nodes", `{"digest":"d1"}`)
	if got := decodeMap(t, rec.Body.Bytes()); got["need_nodes"] != true {
		t.Fatalf("a remembered digest after a removal = %v", got)
	}
	if rec := rig.do(tokens["gpu-1"], http.MethodPost, "/api/v1/slurm/gpu-nodes", full); rec.Code != http.StatusOK {
		t.Fatalf("report = %d %s", rec.Code, rec.Body)
	}
	item = slurmEntryItem(t, rig)
	missing, _ = item["missing_nodes"].([]any)
	if item["slurm_gpu_nodes"] != float64(2) || len(missing) != 1 || missing[0].(map[string]any)["reason"] != "not_enrolled" {
		t.Fatalf("after the next report = %v", item)
	}
	if _, ok := item["list_as_of"]; ok {
		t.Fatalf("an entry with an agent online says its list is old: %v", item)
	}
	// The last agent goes: the list is the one from before.
	set("gpu-1", store.NodeStateOffline)
	if item = slurmEntryItem(t, rig); item["online_count"] != float64(0) || item["list_as_of"] != "2026-11-18T11:20:00Z" {
		t.Fatalf("no agent online = %v", item)
	}
}

// The sweep deletes a node without going through the API (so nothing resets
// the report memory), and its row goes with it. The node is still in the
// cluster, so the next report with the remembered digest stores the list
// again and the row is back (2026-11-18 review: it stayed missing, and the
// node was swept again on its next outage).
func TestAReportAfterTheSweepDeletedANodeStoresTheListAgain(t *testing.T) {
	rig := newTokenRig(t)
	_, tokens := enrolledSlurmNode(t, rig, "gpu-1", "gpu-2")
	if err := rig.st.DB().Model(&store.Node{}).Where("slurm_node = ?", "gpu-1").Update("state", store.NodeStateOnline).Error; err != nil {
		t.Fatal(err)
	}
	full := `{"digest":"d1","nodes":[{"name":"gpu-1","state":"idle","gpus":1},{"name":"gpu-2","state":"idle","gpus":1}]}`
	if rec := rig.do(tokens["gpu-1"], http.MethodPost, "/api/v1/slurm/gpu-nodes", full); rec.Code != http.StatusOK {
		t.Fatalf("report = %d %s", rec.Code, rec.Body)
	}
	var gone store.Node
	if err := rig.st.DB().Where("slurm_node = ?", "gpu-2").First(&gone).Error; err != nil {
		t.Fatal(err)
	}
	if err := rig.st.DeleteNode(gone.UUID); err != nil {
		t.Fatal(err)
	}
	if item := slurmEntryItem(t, rig); item["slurm_gpu_nodes"] != float64(1) {
		t.Fatalf("after the sweep's delete = %v", item)
	}
	rec := rig.do(tokens["gpu-1"], http.MethodPost, "/api/v1/slurm/gpu-nodes", `{"digest":"d1"}`)
	if got := decodeMap(t, rec.Body.Bytes()); got["need_nodes"] != true {
		t.Fatalf("the remembered digest after a row went = %v; want the nodes asked for", got)
	}
	if rec := rig.do(tokens["gpu-1"], http.MethodPost, "/api/v1/slurm/gpu-nodes", full); rec.Code != http.StatusOK {
		t.Fatalf("report = %d %s", rec.Code, rec.Body)
	}
	if item := slurmEntryItem(t, rig); item["slurm_gpu_nodes"] != float64(2) {
		t.Fatalf("after the next report = %v; want gpu-2 back on the list", item)
	}
	// And then nothing changes: the digest and the rows agree again.
	rec = rig.do(tokens["gpu-1"], http.MethodPost, "/api/v1/slurm/gpu-nodes", `{"digest":"d1"}`)
	if got := decodeMap(t, rec.Body.Bytes()); got["need_nodes"] != false && got["need_nodes"] != nil {
		t.Fatalf("a steady report asked for the nodes again: %v", got)
	}
}
