package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const slurmCreateBody = `{"name":"hpc","server_url":"https://gw.example:8800/","sbatch_args":"-A ai --time=7-0"}`

func decodeMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return out
}

// createSlurm makes an entry through the API and returns its UUID and token,
// the token read out of the command the administrator is given.
func createSlurm(t *testing.T, rig *tokenRig, body string) (uuid, token string, created map[string]any) {
	t.Helper()
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	created = decodeMap(t, rec.Body.Bytes())
	cmd, _ := created["setup_command"].(string)
	const prefix = "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token="
	if !strings.HasPrefix(cmd, prefix) || !strings.HasSuffix(cmd, "' | sudo sh") {
		t.Fatalf("setup_command = %q", cmd)
	}
	token, err := url.QueryUnescape(strings.TrimSuffix(strings.TrimPrefix(cmd, prefix), "' | sudo sh"))
	if err != nil || token == "" {
		t.Fatalf("token in the setup command: %q %v", token, err)
	}
	return created["uuid"].(string), token, created
}

func TestSlurmClusterLifecycleOverTheWire(t *testing.T) {
	rig := newTokenRig(t)
	uuid, token, created := createSlurm(t, rig, slurmCreateBody)
	want := map[string]any{
		"name": "hpc", "server_url": "https://gw.example:8800", "slurm_cluster_name": "",
		"run_as": "root", "account_user": "", "install_dir": "/var/lib/gpuworks", "script_dir": "/opt/gpuworks-slurm",
		"sbatch_args": "-A ai --time=7-0", "idle_return_seconds": float64(60), "pending_timeout_seconds": float64(600),
		"revoked": false, "state": "pending", "stalled": false,
		"node_count": float64(0), "slurm_gpu_nodes": float64(0), "missing_count": float64(0), "missing_nodes": []any{},
		"category": "slurm", "resources": []any{map[string]any{"type": "slurm"}},
	}
	for key, w := range want {
		if !reflect.DeepEqual(created[key], w) {
			t.Fatalf("created[%q] = %#v, want %#v (body %v)", key, created[key], w, created)
		}
	}
	for _, key := range []string{"token", "token_hash", "source"} {
		if _, ok := created[key]; ok {
			t.Fatalf("created carries %q", key)
		}
	}
	if !strings.HasSuffix(created["remove_command"].(string), "&token="+url.QueryEscape(token)+"' | sudo sh -s -- --remove") &&
		!strings.Contains(created["remove_command"].(string), "token="+url.QueryEscape(token)) {
		t.Fatalf("remove_command = %v", created["remove_command"])
	}
	if !strings.HasSuffix(created["check_command"].(string), "sudo sh -s -- --check") {
		t.Fatalf("check_command = %v", created["check_command"])
	}
	if !strings.HasSuffix(created["uninstall_agents_command"].(string), "sudo sh -s -- --uninstall-agents") {
		t.Fatalf("uninstall_agents_command = %v", created["uninstall_agents_command"])
	}

	// The scripts are served to the token holder without a login.
	for _, name := range []string{"setup.sh", "check.sh"} {
		rec := rig.do("", http.MethodGet, "/slurm/"+name+"?token="+url.QueryEscape(token), "")
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.HasPrefix(body, "#!/bin/sh") || rec.Header().Get("Content-Type") != "text/x-shellscript; charset=utf-8" ||
			rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s by token = %d %v\n%.200s", name, rec.Code, rec.Header(), body)
		}
		for _, baked := range []string{"https://gw.example:8800", "/var/lib/gpuworks", "/opt/gpuworks-slurm"} {
			if !strings.Contains(body, baked) {
				t.Fatalf("%s does not carry %q", name, baked)
			}
		}
		// Only the setup script holds the token: the check script is copied
		// to every node, where every user can read it, and reads a root-only file.
		if has := strings.Contains(body, token); has != (name == "setup.sh") {
			t.Fatalf("%s carries the token = %v", name, has)
		}
		if rec := rig.do("", http.MethodGet, "/slurm/"+name+"?token=nope", ""); rec.Code != http.StatusNotFound {
			t.Fatalf("%s with a wrong token = %d", name, rec.Code)
		}
		if rec := rig.do("", http.MethodGet, "/slurm/"+name, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("%s without a token = %d", name, rec.Code)
		}
	}

	// Enrollment: idempotent per node, bound to the first Slurm cluster name.
	enroll := func(body string) (int, map[string]any) {
		rec := rig.do("", http.MethodPost, "/api/v1/slurm/enroll", body)
		return rec.Code, decodeMap(t, rec.Body.Bytes())
	}
	tok, _ := json.Marshal(token)
	code, first := enroll(`{"token":` + string(tok) + `,"slurm_cluster_name":"demo","node_name":"gpu-1"}`)
	settings := map[string]any{"sbatch_args": "-A ai --time=7-0", "idle_return_seconds": float64(60), "pending_timeout_seconds": float64(600), "account_user": ""}
	if code != http.StatusOK || first["node_uuid"] == "" || first["node_token"] == "" || !reflect.DeepEqual(first["settings"], settings) || len(first) != 3 {
		t.Fatalf("enroll = %d %v", code, first)
	}
	if code, again := enroll(`{"token":` + string(tok) + `,"slurm_cluster_name":"demo","node_name":"gpu-1"}`); code != http.StatusOK || again["node_uuid"] != first["node_uuid"] || again["node_token"] != first["node_token"] {
		t.Fatalf("re-enroll = %d %v", code, again)
	}
	if code, out := enroll(`{"token":` + string(tok) + `,"slurm_cluster_name":"other","node_name":"gpu-2"}`); code != http.StatusForbidden || out["code"] != ErrorSlurmEnrollRefused ||
		out["params"].(map[string]any)["reason"] != "other_cluster" || out["node_token"] != nil {
		t.Fatalf("other cluster = %d %v", code, out)
	}
	if code, out := enroll(`{"token":"nope","slurm_cluster_name":"demo","node_name":"gpu-2"}`); code != http.StatusForbidden || out["code"] != ErrorSlurmEnrollRefused ||
		out["params"].(map[string]any)["reason"] != "unknown_token" {
		t.Fatalf("unknown token = %d %v", code, out)
	}
	for _, bad := range []string{`{"token":` + string(tok) + `,"slurm_cluster_name":"","node_name":"gpu-2"}`, `{"token":` + string(tok) + `,"slurm_cluster_name":"demo","node_name":""}`, `{`} {
		if code, _ := enroll(bad); code != http.StatusBadRequest {
			t.Fatalf("enroll %s = %d", bad, code)
		}
	}
	// A second entry of the same Slurm cluster is refused a node the first holds.
	_, tokenB, _ := createSlurm(t, rig, `{"name":"hpc-b","server_url":"https://gw.example:8800"}`)
	tb, _ := json.Marshal(tokenB)
	if code, out := enroll(`{"token":` + string(tb) + `,"slurm_cluster_name":"demo","node_name":"gpu-1"}`); code != http.StatusForbidden ||
		out["params"].(map[string]any)["reason"] != "node_taken" || out["params"].(map[string]any)["entry"] != "hpc" {
		t.Fatalf("node held by another entry = %d %v", code, out)
	}

	// The list shows the node and the bound name.
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "")
	var list struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 2 || list.Total != 2 || list.Items[0]["node_count"] != float64(1) ||
		list.Items[0]["slurm_cluster_name"] != "demo" || list.Items[0]["state"] != "active" {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}

	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/slurm/clusters/"+uuid, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorSlurmClusterHasNodes) {
		t.Fatalf("delete with nodes = %d %s", rec.Code, rec.Body)
	}
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/revoke", "")
	revoked := decodeMap(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || revoked["revoked"] != true || revoked["state"] != "revoked" || revoked["setup_command"] != nil || revoked["remove_command"] == nil {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if code, _ := enroll(`{"token":` + string(tok) + `,"slurm_cluster_name":"demo","node_name":"gpu-1"}`); code != http.StatusForbidden {
		t.Fatalf("enroll after revoke = %d", code)
	}
	// The scripts that take the health check out again stay reachable.
	if rec := rig.do("", http.MethodGet, "/slurm/setup.sh?token="+url.QueryEscape(token), ""); rec.Code != http.StatusOK {
		t.Fatalf("setup.sh after revoke = %d (--remove must still work)", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/missing/revoke", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke missing = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/slurm/clusters/missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d", rec.Code)
	}
	// Once its node is gone the entry can be deleted.
	node, err := rig.st.NodeByUUID(first["node_uuid"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/nodes/"+node.UUID, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"deleted"`) {
		t.Fatalf("delete slurm node (needs no SSH) = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/slurm/clusters/"+uuid, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
}

func TestSlurmClusterDefaultsOverTheWire(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/defaults", "")
	const want = `{"run_as":"root","install_dir":"/var/lib/gpuworks","script_dir":"/opt/gpuworks-slurm",` +
		`"idle_return_seconds":60,"idle_return_min_seconds":1,"idle_return_max_seconds":86400,` +
		`"pending_timeout_seconds":600,"pending_timeout_min_seconds":30,"pending_timeout_max_seconds":86400,` +
		`"health_check_interval_seconds":300,"stalled_after_seconds":600,"report_interval_seconds":600,"missing_nodes_limit":100,"quick_tunnel_url":""}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("defaults = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do("", http.MethodGet, "/api/v1/slurm/defaults", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous defaults = %d", rec.Code)
	}
}

func TestSlurmClusterValidation(t *testing.T) {
	rig := newTokenRig(t)
	for _, tc := range []struct{ body, field string }{
		{`{"name":"","server_url":"https://gw"}`, "name"},
		{`{"name":"a","server_url":"gw"}`, "server_url"},
		{`{"name":"a","server_url":"https://gw/'; rm -rf /"}`, "server_url"},
		{`{"name":"a","server_url":"https://gw","run_as":"Bad User"}`, "run_as"},
		{`{"name":"a","server_url":"https://gw","install_dir":"var/lib/x"}`, "install_dir"},
		{`{"name":"a","server_url":"https://gw","script_dir":"/opt/../etc"}`, "script_dir"},
		{`{"name":"a","server_url":"https://gw","account_user":"Root!"}`, "account_user"},
		{`{"name":"a","server_url":"https://gw","sbatch_args":"-A x\n--wrap=evil"}`, "sbatch_args"},
		{`{"name":"a","server_url":"https://gw","idle_return_seconds":0}`, "idle_return_seconds"},
		{`{"name":"a","server_url":"https://gw","idle_return_seconds":86401}`, "idle_return_seconds"},
		{`{"name":"a","server_url":"https://gw","pending_timeout_seconds":29}`, "pending_timeout_seconds"},
		{`{"name":"a","server_url":"https://gw","provider_id":9}`, "provider_cluster_id"},
		{`{"name":"a","server_url":"https://gw","provider_cluster_id":"ehpc-1"}`, "provider_cluster_id"},
		{`{"name":"a","server_url":"https://gw","provider_id":9,"provider_cluster_id":"ehpc-1"}`, "provider_id"},
	} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorSlurmClusterInvalid) || !strings.Contains(rec.Body.String(), `"field":"`+tc.field+`"`) {
			t.Fatalf("%s: %d %s", tc.body, rec.Code, rec.Body)
		}
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters", slurmCreateBody); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters", slurmCreateBody); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorSlurmClusterNameTaken) {
		t.Fatalf("duplicate = %d %s", rec.Code, rec.Body)
	}
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters",
		`{"name":"b","server_url":"https://gw","run_as":"gpusvc","account_user":"svc","install_dir":"/data/gw","script_dir":"/shared/gw","idle_return_seconds":300,"pending_timeout_seconds":1200}`)
	b := decodeMap(t, rec.Body.Bytes())
	if rec.Code != http.StatusCreated || b["run_as"] != "gpusvc" || b["account_user"] != "svc" || b["install_dir"] != "/data/gw" || b["script_dir"] != "/shared/gw" ||
		b["idle_return_seconds"] != float64(300) || b["pending_timeout_seconds"] != float64(1200) {
		t.Fatalf("explicit settings = %d %s", rec.Code, rec.Body)
	}
	// A cloud cluster entry needs a real account, and is made once.
	acct := &store.Provider{Name: "ali", Kind: "aliyun"}
	if err := rig.st.DB().Create(acct).Error; err != nil {
		t.Fatal(err)
	}
	body := `{"name":"ehpc","server_url":"https://gw","provider_id":` + jsonUint(acct.ID) + `,"provider_cluster_id":"ehpc-1"}`
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters", body)
	src, _ := decodeMap(t, rec.Body.Bytes())["source"].(map[string]any)
	if rec.Code != http.StatusCreated || src["provider_cluster_id"] != "ehpc-1" || src["provider_id"] != float64(acct.ID) {
		t.Fatalf("cloud entry = %d %s", rec.Code, rec.Body)
	}
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters", strings.Replace(body, `"ehpc"`, `"ehpc2"`, 1))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorSlurmClusterSourceTaken) {
		t.Fatalf("second entry for one cloud cluster = %d %s", rec.Code, rec.Body)
	}
}

func TestSlurmClusterAccess(t *testing.T) {
	rig := newTokenRig(t)
	uuid, _, _ := createSlurm(t, rig, slurmCreateBody)
	if rec := rig.do("", http.MethodGet, "/api/v1/slurm/clusters", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d", rec.Code)
	}
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	user, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: 2, Username: "user", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/slurm/clusters", slurmCreateBody},
		{http.MethodPut, "/api/v1/slurm/clusters/" + uuid, `{"name":"x"}`},
		{http.MethodGet, "/api/v1/slurm/clusters/" + uuid + "/setup", ""},
		{http.MethodPost, "/api/v1/slurm/clusters/" + uuid + "/revoke", ""},
		{http.MethodDelete, "/api/v1/slurm/clusters/" + uuid, ""},
	} {
		if rec := rig.do(user, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Fatalf("user %s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
		if rec := rig.do("", tc.method, tc.path, tc.body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
	rec := rig.do(user, http.MethodGet, "/api/v1/slurm/clusters", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "_command") || strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("user list = %d %s; the token-bearing commands are for administrators", rec.Code, rec.Body)
	}
	// Wrong methods on the routes.
	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/api/v1/slurm/enroll"}, {http.MethodGet, "/api/v1/slurm/enroll"}, {http.MethodPost, "/slurm/setup.sh"},
		{http.MethodPost, "/slurm/check.sh"}, {http.MethodPost, "/slurm/token-status"}, {http.MethodGet, "/api/v1/slurm/clusters/" + uuid + "/apply"},
		{http.MethodGet, "/api/v1/slurm/clusters/" + uuid + "/remove"}, {http.MethodPatch, "/api/v1/slurm/clusters/" + uuid}, {http.MethodGet, "/api/v1/slurm/gpu-nodes"},
	} {
		if rec := rig.do(rig.session, tc.method, tc.path, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
		}
	}
}

func TestSlurmClusterSetupCommands(t *testing.T) {
	rig := newTokenRig(t)
	uuid, token, _ := createSlurm(t, rig, slurmCreateBody)
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters/"+uuid+"/setup", "")
	got := decodeMap(t, rec.Body.Bytes())
	q := url.QueryEscape(token)
	want := map[string]any{
		"setup_command":            "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token=" + q + "' | sudo sh",
		"check_command":            "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token=" + q + "&check=1' | sudo sh -s -- --check",
		"remove_command":           "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token=" + q + "' | sudo sh -s -- --remove",
		"uninstall_agents_command": "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token=" + q + "' | sudo sh -s -- --uninstall-agents",
		"setup_url":                "https://gw.example:8800/slurm/setup.sh?token=" + q,
		"check_script_url":         "https://gw.example:8800/slurm/check.sh?token=" + q,
		"default_interval_seconds": float64(300),
	}
	if rec.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("setup = %d %v\nwant %v", rec.Code, got, want)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters/missing/setup", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("setup of a missing entry = %d", rec.Code)
	}
	rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/revoke", "")
	got = decodeMap(t, rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters/"+uuid+"/setup", "").Body.Bytes())
	if got["setup_command"] != nil || got["check_command"] != nil || got["setup_url"] != nil || got["remove_command"] == nil || got["uninstall_agents_command"] == nil {
		t.Fatalf("setup of a revoked entry = %v", got)
	}
}

func TestSlurmNodeDeleteNeedsNoSSH(t *testing.T) {
	rig := newTokenRig(t)
	c, err := rig.st.CreateSlurmCluster(store.SlurmCluster{Name: "c"})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := rig.st.EnrollSlurmNode(c.Token, "demo", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	rec := rig.do(rig.session, http.MethodDelete, "/api/v1/nodes/"+node.UUID, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"deleted"`) {
		t.Fatalf("delete slurm node = %d %s", rec.Code, rec.Body)
	}
}

// A node of a Slurm entry says so in its source: resource "slurm", the entry
// as its cluster; the node list filters by it like any cluster.
func TestSlurmNodeSourceOverTheWire(t *testing.T) {
	rig := newTokenRig(t)
	uuid, tokens := enrolledSlurmNode(t, rig, "gpu-1")
	_ = tokens
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/nodes?source=cluster:"+uuid, "")
	var list struct {
		Items []struct {
			Name   string         `json:"name"`
			Source map[string]any `json:"source"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("nodes of the entry = %d %s", rec.Code, rec.Body)
	}
	want := map[string]any{"resource": "slurm", "cluster_uuid": uuid, "cluster_name": "hpc"}
	if list.Items[0].Name != "gpu-1" || !reflect.DeepEqual(list.Items[0].Source, want) {
		t.Fatalf("source = %v, want %v", list.Items[0].Source, want)
	}
}

// The assistant reads the list as the administrator who asked but never sees
// the commands: they carry the enrollment token.
func TestSlurmClusterListHidesTheCommandsFromTheAssistant(t *testing.T) {
	rig := newTokenRig(t)
	createSlurm(t, rig, slurmCreateBody)
	read := func(assistant bool) string {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/slurm/clusters", nil)
		req.Header.Set("Authorization", "Bearer "+rig.session)
		if assistant {
			req = req.WithContext(context.WithValue(req.Context(), assistantReadKey{}, true))
		}
		rec := httptest.NewRecorder()
		rig.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	if body := read(false); !strings.Contains(body, "setup_command") || !strings.Contains(body, "token=") {
		t.Fatalf("the administrator's list lacks the commands: %s", body)
	}
	if body := read(false); !strings.Contains(body, "check_command") {
		t.Fatalf("the administrator's list lacks the check command: %s", body)
	}
	if body := read(true); strings.Contains(body, "_command") || strings.Contains(body, "token=") {
		t.Fatalf("the assistant's list carries the enrollment token: %s", body)
	}
}

// The health check asks, without a login, whether its token is still the
// token of a live entry: the answer is one word and nothing else.
func TestSlurmTokenStatusOverTheWire(t *testing.T) {
	rig := newTokenRig(t)
	uuid, token, _ := createSlurm(t, rig, slurmCreateBody)
	ask := func(path string) (int, string, http.Header) {
		rec := rig.do("", http.MethodGet, path, "")
		return rec.Code, strings.TrimSpace(rec.Body.String()), rec.Header()
	}
	code, body, header := ask("/slurm/token-status?token=" + url.QueryEscape(token))
	if code != http.StatusOK || body != `{"status":"active"}` || header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(header.Get("Content-Type"), "application/json") {
		t.Fatalf("active = %d %s %v", code, body, header)
	}
	for _, path := range []string{"/slurm/token-status?token=nope", "/slurm/token-status?token=", "/slurm/token-status", "/slurm/token-status?token=" + strings.Repeat("a", 4096)} {
		if code, body, _ := ask(path); code != http.StatusOK || body != `{"status":"unknown"}` {
			t.Fatalf("%s = %d %s", path, code, body)
		}
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/revoke", ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if code, body, _ := ask("/slurm/token-status?token=" + url.QueryEscape(token)); code != http.StatusOK || body != `{"status":"revoked"}` {
		t.Fatalf("revoked = %d %s", code, body)
	}
	if rec := rig.do("", http.MethodPost, "/slurm/token-status?token="+url.QueryEscape(token), ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
}

func TestSlurmClusterDTOCarriesWhatDeletingWithItsNodesNeeds(t *testing.T) {
	rig := newTokenRig(t)
	uuid, tokens := enrolledSlurmNode(t, rig, "gpu-1", "gpu-2")
	_ = tokens
	entry := func() map[string]any {
		var list struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "").Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
			t.Fatalf("list: %v", err)
		}
		return list.Items[0]
	}
	// Nodes enrolled and not online: the entry can go with them.
	if e := entry(); e["delete_with_nodes_allowed"] != true || e["delete_with_nodes_blocker"] != nil || e["node_count"] != float64(2) {
		t.Fatalf("offline nodes: %v", e)
	}
	setState := func(node, state string) {
		if err := rig.st.DB().Model(&store.Node{}).Where("slurm_node = ?", node).Update("state", state).Error; err != nil {
			t.Fatal(err)
		}
	}
	setState("gpu-1", store.NodeStateOnline)
	if e := entry(); e["delete_with_nodes_allowed"] != false || e["delete_with_nodes_blocker"] != "online" {
		t.Fatalf("an agent online: %v", e)
	}
	setState("gpu-1", store.NodeStateOffline)
	holdInstance(t, rig.st, "gpu-2", "running")
	if e := entry(); e["delete_with_nodes_allowed"] != false || e["delete_with_nodes_blocker"] != "instances" {
		t.Fatalf("a node with an instance: %v", e)
	}
	// An instance only waiting to be removed does not hold the node.
	rig.st.DB().Model(&store.Instance{}).Where("1 = 1").Update("state", "removing")
	if e := entry(); e["delete_with_nodes_allowed"] != true {
		t.Fatalf("a leaving instance blocks: %v", e)
	}
	_ = uuid
}

func holdInstance(t *testing.T, st *store.Store, slurmNode, state string) {
	t.Helper()
	var n store.Node
	if err := st.DB().Where("slurm_node = ?", slurmNode).First(&n).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Instance{UUID: "inst-" + slurmNode, NodeID: n.ID, State: state}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestDeleteSlurmClusterWithItsOfflineNodes(t *testing.T) {
	rig := newTokenRig(t)
	uuid, _ := enrolledSlurmNode(t, rig, "gpu-1", "gpu-2")
	del := func(query string) *httptest.ResponseRecorder {
		return rig.do(rig.session, http.MethodDelete, "/api/v1/slurm/clusters/"+uuid+query, "")
	}
	nodeCount := func() int64 {
		var n int64
		rig.st.DB().Model(&store.Node{}).Where("slurm_cluster_id > 0").Count(&n)
		return n
	}
	// Without the option it still refuses, as before; so does a false one.
	for _, q := range []string{"", "?with_offline_nodes=false"} {
		if rec := del(q); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorSlurmClusterHasNodes) {
			t.Fatalf("delete%s = %d %s", q, rec.Code, rec.Body)
		}
	}
	for _, q := range []string{"?with_offline_nodes=maybe", "?with_offline_nodes=true&with_offline_nodes=true"} {
		if rec := del(q); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"with_offline_nodes"`) {
			t.Fatalf("delete%s = %d %s", q, rec.Code, rec.Body)
		}
	}
	// An online agent blocks, and nothing is deleted.
	if err := rig.st.DB().Model(&store.Node{}).Where("slurm_node = ?", "gpu-1").Update("state", store.NodeStateOnline).Error; err != nil {
		t.Fatal(err)
	}
	rec := del("?with_offline_nodes=true")
	got := decodeMap(t, rec.Body.Bytes())
	if rec.Code != http.StatusConflict || got["code"] != messages.ErrorSlurmClusterNodesOnline || !reflect.DeepEqual(got["params"], map[string]any{"count": float64(1)}) || nodeCount() != 2 {
		t.Fatalf("online = %d %s (nodes left %d)", rec.Code, rec.Body, nodeCount())
	}
	// So does an instance on an offline node.
	rig.st.DB().Model(&store.Node{}).Where("slurm_node = ?", "gpu-1").Update("state", store.NodeStateOffline)
	holdInstance(t, rig.st, "gpu-2", "running")
	rec = del("?with_offline_nodes=true")
	got = decodeMap(t, rec.Body.Bytes())
	if rec.Code != http.StatusConflict || got["code"] != messages.ErrorSlurmClusterNodesHaveInstances || !reflect.DeepEqual(got["params"], map[string]any{"count": float64(1)}) || nodeCount() != 2 {
		t.Fatalf("instances = %d %s (nodes left %d)", rec.Code, rec.Body, nodeCount())
	}
	// Offline and idle: the entry goes with its nodes (and the rows only waiting to leave).
	rig.st.DB().Model(&store.Instance{}).Where("uuid = ?", "inst-gpu-2").Update("state", "removing")
	if rec := del("?with_offline_nodes=true"); rec.Code != http.StatusNoContent {
		t.Fatalf("delete with nodes = %d %s", rec.Code, rec.Body)
	}
	if nodeCount() != 0 {
		t.Fatalf("nodes left = %d", nodeCount())
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", ""); strings.Contains(rec.Body.String(), uuid) {
		t.Fatalf("the entry is still listed: %s", rec.Body)
	}
	if rec := del("?with_offline_nodes=true"); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again = %d", rec.Code)
	}
	// The option on an entry without nodes is a plain delete.
	uuid2, _, _ := createSlurm(t, rig, `{"name":"empty","server_url":"https://gw.example:8800"}`)
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/slurm/clusters/"+uuid2+"?with_offline_nodes=true", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete an empty entry with the option = %d %s", rec.Code, rec.Body)
	}
}

// The entry is revoked before its nodes are deleted: while a node is being
// deleted the health check on its machine must not be able to enroll it again
// with the entry's token. And a deletion that is refused revokes nothing.
func TestDeleteSlurmClusterWithNodesRevokesTheEntryBeforeDeletingNodes(t *testing.T) {
	rig := newTokenRig(t)
	uuid, tokens := enrolledSlurmNode(t, rig, "gpu-1", "gpu-2")
	_ = tokens
	cluster, err := rig.st.SlurmClusterByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	// A node with an instance refuses the deletion, and the entry stays usable.
	holdInstance(t, rig.st, "gpu-2", "running")
	var enrollErrs []error
	a := &apiHandlers{d: Deps{Store: rig.st, KickNode: func(string) {
		// Called after each node is deleted, before the entry is.
		_, _, err := rig.st.EnrollSlurmNode(cluster.Token, "demo", "gpu-9")
		enrollErrs = append(enrollErrs, err)
	}}}
	r := chi.NewRouter()
	r.Delete("/slurm/clusters/{uuid}", a.deleteSlurmCluster)
	del := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/slurm/clusters/"+uuid+"?with_offline_nodes=true", nil))
		return rec
	}
	if rec := del(); rec.Code != http.StatusConflict {
		t.Fatalf("delete with an instance = %d %s", rec.Code, rec.Body)
	}
	if c, err := rig.st.SlurmClusterByUUID(uuid); err != nil || c.RevokedAt != nil {
		t.Fatalf("a refused deletion revoked the entry: %+v %v", c, err)
	}
	rig.st.DB().Model(&store.Instance{}).Where("1 = 1").Update("state", "removing")
	if rec := del(); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if len(enrollErrs) != 2 {
		t.Fatalf("nodes kicked %d times, want 2", len(enrollErrs))
	}
	for i, err := range enrollErrs {
		if !errors.Is(err, store.ErrSlurmTokenUnknown) {
			t.Fatalf("while node %d was deleted the entry's token still enrolled a node: %v", i+1, err)
		}
	}
}

// An administrator reads the setup script the entry's controller is served:
// as the script is served, minus the enrollment token, which the text does not
// carry. An unknown entry and a missing login are refused.
func TestSlurmClusterScriptIsServedToAnAdministratorWithoutTheToken(t *testing.T) {
	rig := newTokenRig(t)
	uuid, token, _ := createSlurm(t, rig, slurmCreateBody)
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters/"+uuid+"/script", "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/x-shellscript") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("script = %d %v", rec.Code, rec.Header())
	}
	body := rec.Body.String()
	if strings.Contains(body, token) {
		t.Fatal("the script text carries the enrollment token")
	}
	for _, want := range []string{"#!/bin/sh", "TOKEN='REDACTED-ENROLLMENT-TOKEN'", "SERVER_URL='https://gw.example:8800'", "--check", "GWKS_CHECK_SCRIPT_EOF"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the script lacks %q", want)
		}
	}
	// What the controller is served differs from it by the token alone.
	served := rig.do("", http.MethodGet, "/slurm/setup.sh?token="+url.QueryEscape(token), "").Body.String()
	if strings.ReplaceAll(served, token, slurmScriptRedacted) != body {
		t.Fatal("the script shown is not the script served, token aside")
	}
	// A revoked entry still has its script, which uninstalling needs.
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/revoke", ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters/"+uuid+"/script", ""); rec.Code != http.StatusOK {
		t.Fatalf("revoked entry's script = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters/nope/script", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown entry = %d", rec.Code)
	}
	if rec := rig.do("", http.MethodGet, "/api/v1/slurm/clusters/"+uuid+"/script", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/script", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
}
