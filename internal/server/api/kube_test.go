package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

const kubeCreateBody = `{"name":"prod","server_url":"https://gw.example:8800/","node_selector":"gpuworks.ai/pool=a","agent_image":"reg.example/gpuworks-agent:v1"}`

func TestKubeClusterLifecycleOverTheWire(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"name": "prod", "namespace": "gpuworks-system", "server_url": "https://gw.example:8800",
		"agent_image": "reg.example/gpuworks-agent:v1", "node_selector": "gpuworks.ai/pool=a",
		"runtime_class": "nvidia", "containerd_socket": "/run/containerd/containerd.sock",
		"engine_ports": "40000-42767", "idle_return_seconds": float64(600), "cluster_uid": "",
		"revoked": false, "node_count": float64(0),
	} {
		if created[key] != want {
			t.Fatalf("created[%q] = %#v, want %#v (body %s)", key, created[key], want, rec.Body)
		}
	}
	if _, leaked := created["token"]; leaked {
		t.Fatal("the enrollment token is a field of its own on the wire")
	}
	apply, _ := created["apply_command"].(string)
	const prefix = "kubectl apply -f 'https://gw.example:8800/kube/manifest.yaml?token="
	if !strings.HasPrefix(apply, prefix) || !strings.HasSuffix(apply, "'") {
		t.Fatalf("apply_command = %q", apply)
	}
	token, err := url.QueryUnescape(strings.TrimSuffix(strings.TrimPrefix(apply, prefix), "'"))
	if err != nil || token == "" {
		t.Fatalf("token in apply command: %q %v", token, err)
	}
	uuid := created["uuid"].(string)

	// The manifest is served to the token holder without a login.
	if rec := rig.do("", http.MethodGet, "/kube/manifest.yaml?token="+url.QueryEscape(token), ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "kind: DaemonSet") || rec.Header().Get("Content-Type") != "application/yaml; charset=utf-8" {
		t.Fatalf("manifest by token = %d %s", rec.Code, rec.Header())
	}
	if rec := rig.do("", http.MethodGet, "/kube/manifest.yaml?token=nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("manifest with a wrong token = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+uuid+"/manifest", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin manifest = %d", rec.Code)
	}

	// Enrollment: idempotent per node, bound to the first cluster UID.
	enroll := func(body string) (int, map[string]any) {
		rec := rig.do("", http.MethodPost, "/api/v1/kube/enroll", body)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	tok, _ := json.Marshal(token)
	code, first := enroll(`{"token":` + string(tok) + `,"cluster_uid":"uid-a","node_name":"gpu-1"}`)
	if code != http.StatusOK || first["node_uuid"] == "" || first["node_token"] == "" || first["idle_return_seconds"] != float64(600) {
		t.Fatalf("enroll = %d %v", code, first)
	}
	if code, again := enroll(`{"token":` + string(tok) + `,"cluster_uid":"uid-a","node_name":"gpu-1"}`); code != http.StatusOK || again["node_uuid"] != first["node_uuid"] || again["node_token"] != first["node_token"] {
		t.Fatalf("re-enroll = %d %v", code, again)
	}
	if code, out := enroll(`{"token":` + string(tok) + `,"cluster_uid":"uid-b","node_name":"gpu-2"}`); code != http.StatusForbidden || out["code"] != ErrorKubeEnrollRefused || out["params"].(map[string]any)["reason"] != "other_cluster" {
		t.Fatalf("other cluster = %d %v", code, out)
	}
	if code, out := enroll(`{"token":"nope","cluster_uid":"uid-a","node_name":"gpu-2"}`); code != http.StatusForbidden || out["params"].(map[string]any)["reason"] != "unknown_token" {
		t.Fatalf("unknown token = %d %v", code, out)
	}
	if code, _ := enroll(`{"token":` + string(tok) + `,"cluster_uid":"","node_name":"gpu-2"}`); code != http.StatusBadRequest {
		t.Fatalf("missing cluster uid = %d", code)
	}

	// The node shows up in the list with the cluster's count.
	rec = rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 || list.Items[0]["node_count"] != float64(1) || list.Items[0]["cluster_uid"] != "uid-a" {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}

	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/kube/clusters/"+uuid, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorKubeClusterHasNodes) {
		t.Fatalf("delete with nodes = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters/"+uuid+"/revoke", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revoked":true`) || strings.Contains(rec.Body.String(), "apply_command") {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if code, _ := enroll(`{"token":` + string(tok) + `,"cluster_uid":"uid-a","node_name":"gpu-1"}`); code != http.StatusForbidden {
		t.Fatalf("enroll after revoke = %d", code)
	}
	if rec := rig.do("", http.MethodGet, "/kube/manifest.yaml?token="+url.QueryEscape(token), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("manifest after revoke = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters/missing/revoke", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke missing = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/kube/clusters/missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d", rec.Code)
	}
}

func TestKubeClusterValidation(t *testing.T) {
	rig := newTokenRig(t)
	for _, tc := range []struct{ body, field string }{
		{`{"name":"","server_url":"https://gw"}`, "name"},
		{`{"name":"a","server_url":"gw"}`, "manifest"},
		{`{"name":"a","server_url":"https://gw","engine_ports":"9-1"}`, "engine_ports"},
		{`{"name":"a","server_url":"https://gw","idle_return_seconds":-2}`, "idle_return_seconds"},
		{`{"name":"a","server_url":"https://gw","namespace":"Bad_NS"}`, "manifest"},
		{`{"name":"a","server_url":"https://gw","node_selector":"novalue"}`, "manifest"},
	} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorKubeClusterInvalid) || !strings.Contains(rec.Body.String(), `"field":"`+tc.field+`"`) {
			t.Fatalf("%s: %d %s", tc.body, rec.Code, rec.Body)
		}
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorKubeClusterNameTaken) {
		t.Fatalf("duplicate = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"b","server_url":"https://gw","runtime_class":"","idle_return_seconds":-1}`); rec.Code != http.StatusCreated ||
		!strings.Contains(rec.Body.String(), `"runtime_class":""`) || !strings.Contains(rec.Body.String(), `"idle_return_seconds":-1`) {
		t.Fatalf("explicit empty runtime class and keep = %d %s", rec.Code, rec.Body)
	}
}

func TestKubeClusterAccess(t *testing.T) {
	rig := newTokenRig(t)
	if rec := rig.do("", http.MethodGet, "/api/v1/kube/clusters", ""); rec.Code != http.StatusUnauthorized {
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
	if rec := rig.do(user, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody); rec.Code != http.StatusForbidden {
		t.Fatalf("user create = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	rec := rig.do(user, http.MethodGet, "/api/v1/kube/clusters", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "apply_command") {
		t.Fatalf("user list = %d %s; the token-bearing command is for administrators", rec.Code, rec.Body)
	}
	if rec := rig.do(user, http.MethodGet, "/api/v1/kube/clusters/x/manifest", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("user manifest = %d", rec.Code)
	}
}
