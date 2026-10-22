package zstackcloud_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

func cloudInventoryFixture(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validCloudAccessKey(r) {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/v1/hosts/kvm/actions" {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"code":"ID.1002","details":"permission denied"}}`)
			return
		}
		if r.Method != "GET" {
			t.Errorf("installation mutated Cloud: %s", r.URL)
			http.Error(w, "read only", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hosts":
			io.WriteString(w, `{"inventories":[{"uuid":"host-a","name":"Host A","managementIp":"192.0.2.10","hypervisorType":"KVM","status":"Connected","state":"Enabled"},{"uuid":"host-b","name":"Host B","managementIp":"192.0.2.11","hypervisorType":"KVM","status":"Connected","state":"Enabled"}]}`)
		case "/v1/gpu-device/gpu-devices":
			io.WriteString(w, `{"inventories":[{"uuid":"gpu-a","hostUuid":"host-a","pciDeviceAddress":"0000:01:00.0","state":"Enabled","allocateStatus":"Unallocated"},{"uuid":"gpu-b","hostUuid":"host-b","pciDeviceAddress":"0000:02:00.0","state":"Enabled","allocateStatus":"Allocated"}]}`)
		case "/v1/resource-configurations/host-a/kvm/reservedMemory", "/v1/resource-configurations/host-b/kvm/reservedMemory":
			io.WriteString(w, `{"value":"4G","effectiveConfigs":[]}`)
		case "/v1/resource-configurations/host-a/kvm/reservedCpu", "/v1/resource-configurations/host-b/kvm/reservedCpu":
			io.WriteString(w, `{"value":"2","effectiveConfigs":[]}`)
		default:
			t.Errorf("unexpected Cloud path %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func requestJSON(t *testing.T, env *testenv.Env, method, path, body string, status int) []byte {
	t.Helper()
	req, _ := http.NewRequest(method, env.URL()+path, bytes.NewBufferString(body))
	resp, err := env.API(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, resp.StatusCode, status, raw)
	}
	return raw
}

func addedProvider(t *testing.T, env *testenv.Env, endpoint string) string {
	t.Helper()
	raw := requestJSON(t, env, "POST", "/api/v1/providers", fmt.Sprintf(`{"kind":"zstack","name":"Cloud","endpoint":%q,"public_key":"test-access-key","private_key":"test-access-secret"}`, endpoint), 201)
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("/api/v1/providers/%d", created.ID)
}

func TestZStackCloudInstallationRejectsInvalidRequests(t *testing.T) {
	cloud := cloudInventoryFixture(t)
	env := newCloudEnv(t, installerFunc(func(context.Context, provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
		t.Error("invalid request reached SSH installer")
		return provision.SSHInstallReport{}, errors.New("unexpected install")
	}))
	// Missing credentials are invalid only when no platform key is available.
	if err := env.Store().DB().Exec("DELETE FROM platform_ssh_keys").Error; err != nil {
		t.Fatal(err)
	}
	path := addedProvider(t, env, cloud.URL)
	requestJSON(t, env, "POST", path+"/sync", `{}`, 200)
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing URL", `{"external_id":"host-a","password":"secret"}`, 400},
		{"missing credentials", fmt.Sprintf(`{"external_id":"host-a","server_url":%q}`, env.URL()), http.StatusFailedDependency},
		{"invalid key with password", fmt.Sprintf(`{"external_id":"host-a","password":"secret","ssh_key_pem":"secret","server_url":%q}`, env.URL()), http.StatusFailedDependency},
		{"invalid private key", fmt.Sprintf(`{"external_id":"host-a","ssh_key_pem":"not-a-private-key","server_url":%q}`, env.URL()), http.StatusFailedDependency},
		{"invalid connection", fmt.Sprintf(`{"external_id":"host-a","password":"secret","ssh_connection_id":"proxy","server_url":%q}`, env.URL()), http.StatusFailedDependency},
		{"invalid URL", `{"external_id":"host-a","password":"secret","server_url":"file:///tmp/file"}`, 400},
		{"missing host", fmt.Sprintf(`{"external_id":"missing","password":"secret","server_url":%q}`, env.URL()), http.StatusFailedDependency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := requestJSON(t, env, "POST", path+"/install", tc.body, tc.status)
			if bytes.Contains(raw, []byte("secret")) {
				t.Fatal("response leaked credential")
			}
		})
	}
	req, _ := http.NewRequest("POST", env.URL()+path+"/install", bytes.NewBufferString(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated install = %d", resp.StatusCode)
	}
	requestJSON(t, env, "POST", "/api/v1/providers/onboard", `{}`, 405)
	requestJSON(t, env, "GET", path+"/onboarding", ``, 404)
	requestJSON(t, env, "PATCH", path+"/install", `{}`, 405)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 0 {
		t.Fatalf("invalid request created nodes: %v %v", nodes, err)
	}
}

func TestZStackCloudPerHostCredentialsAndSynchronousFailure(t *testing.T) {
	cloud := cloudInventoryFixture(t)
	var mu sync.Mutex
	installed := map[string]string{}
	env := newCloudEnv(t, installerFunc(func(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
		mu.Lock()
		installed[req.Target] = req.Password
		mu.Unlock()
		if req.Target == "192.0.2.10" {
			return provision.SSHInstallReport{}, errors.New("remote installation failed: sensitive-output")
		}
		return provision.SSHInstallReport{Host: req.Target}, nil
	}))
	path := addedProvider(t, env, cloud.URL)
	requestJSON(t, env, "POST", path+"/sync", `{}`, 200)
	requestJSON(t, env, "POST", path+"/install", fmt.Sprintf(`{"external_id":"host-a","password":"secret-a","server_url":%q}`, env.URL()), http.StatusFailedDependency)
	requestJSON(t, env, "POST", path+"/install", fmt.Sprintf(`{"external_id":"host-b","password":"secret-b","server_url":%q}`, env.URL()), 200)
	mu.Lock()
	if installed["192.0.2.10"] != "secret-a" || installed["192.0.2.11"] != "secret-b" {
		t.Errorf("per-host credentials were mixed")
	}
	mu.Unlock()
	raw := requestJSON(t, env, "GET", path+"/instances", ``, 200)
	var rows []struct {
		ExternalID string `json:"external_id"`
		NodeID     uint   `json:"node_id"`
	}
	if err := apiwire.UnmarshalList(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ExternalID == "host-a" && row.NodeID != 0 {
			t.Fatal("failed installation kept a linked node")
		}
		if row.ExternalID == "host-b" {
			node, err := env.Store().NodeByID(row.NodeID)
			if err != nil || node.State != store.NodeStateConnecting {
				t.Fatal("installed agent must wait for registration")
			}
		}
	}
	if bytes.Contains(raw, []byte(`"installation"`)) {
		t.Fatal("instance list retained asynchronous installation status")
	}
	for _, secret := range []string{"secret-a", "secret-b", "sensitive-output"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("instance list leaked credential or remote output")
		}
	}
	requestJSON(t, env, "POST", path+"/install", fmt.Sprintf(`{"external_id":"host-a","password":"secret-a","server_url":%q}`, env.URL()), http.StatusFailedDependency)
	var gpuCount int64
	if err := env.Store().DB().Table("gpus").Count(&gpuCount).Error; err != nil {
		t.Fatal(err)
	}
	if gpuCount != 0 {
		t.Fatalf("unregistered hosts created GPU rows: %d", gpuCount)
	}
}
