package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func nodeNameRouteFixture(t *testing.T, installer *fakeInstaller) (*store.Store, func(string, string, any) *httptest.ResponseRecorder) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "nodes.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, err := auth.NewTokenSigner("node-name-boundary-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	enableTestCredentialEncryption(t, st)
	router := Router(Deps{Store: st, AuthSigner: signer, NodeInstaller: installer})
	return st, func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
}

func TestNodeNameRoutesEnforceTrimmedUTF8ByteBoundary(t *testing.T) {
	for _, operation := range []string{"create", "rename", "install"} {
		t.Run(operation, func(t *testing.T) {
			installer := &fakeInstaller{}
			st, call := nodeNameRouteFixture(t, installer)
			for _, test := range []struct {
				name, input, want string
				invalidText       bool
				rejected          bool
			}{
				{name: "ASCII255", input: strings.Repeat("a", 255), want: strings.Repeat("a", 255)},
				{name: "ASCII256", input: strings.Repeat("a", 256), rejected: true},
				{name: "Chinese255Bytes", input: strings.Repeat("汉", 85), want: strings.Repeat("汉", 85)},
				{name: "Chinese258Bytes", input: strings.Repeat("汉", 86), rejected: true},
				{name: "TrimmedASCII255", input: "  " + strings.Repeat("a", 255) + "  ", want: strings.Repeat("a", 255)},
				{name: "TrimmedChinese255Bytes", input: "　" + strings.Repeat("汉", 85) + "　", want: strings.Repeat("汉", 85)},
				{name: "TrimmedChinese258Bytes", input: " " + strings.Repeat("汉", 86) + " ", rejected: true},
				{name: "LeadingControl", input: "\tvalid", rejected: true, invalidText: true},
				{name: "EmbeddedControl", input: "valid\x00name", rejected: true, invalidText: true},
				{name: "TrailingControl", input: "valid\n", rejected: true, invalidText: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					method, path := http.MethodPost, "/api/v1/nodes"
					var nodeUUID string
					if operation == "rename" {
						seed := call(http.MethodPost, "/api/v1/nodes", map[string]string{"name": "keep-this-name", "server_url": "https://control.example"})
						if seed.Code != http.StatusCreated {
							t.Fatalf("seed status=%d body=%s", seed.Code, seed.Body)
						}
						var wire struct {
							Node struct {
								UUID string `json:"uuid"`
							} `json:"node"`
						}
						if err := json.Unmarshal(seed.Body.Bytes(), &wire); err != nil || wire.Node.UUID == "" {
							t.Fatalf("seed node missing: %v", err)
						}
						nodeUUID = wire.Node.UUID
						method, path = http.MethodPatch, "/api/v1/nodes/"+nodeUUID+"/name"
					}
					body := map[string]any{"name": test.input, "server_url": "https://control.example"}
					if operation == "install" {
						path = "/api/v1/nodes/install"
						body["target"], body["password"], body["no_systemd"] = "root@example.com", "fixture-password", true
					}
					installer.req = provision.SSHInstallRequest{}
					beforeInstaller := installer.req
					before := call(http.MethodGet, "/api/v1/nodes", nil)
					if before.Code != http.StatusOK {
						t.Fatalf("inventory before status=%d", before.Code)
					}
					sub := st.Bus.Subscribe(4, "node")
					defer sub.Close()
					rec := call(method, path, body)
					if test.rejected {
						if rec.Code != http.StatusBadRequest {
							t.Errorf("rejected name status=%d body=%s, want 400", rec.Code, rec.Body)
						}
						var actual map[string]any
						if err := json.Unmarshal(rec.Body.Bytes(), &actual); err != nil {
							t.Fatal(err)
						}
						detail := "must contain at most 255 UTF-8 bytes after trimming whitespace"
						if test.invalidText {
							detail = "must contain valid text without control characters"
						}
						want := map[string]any{
							"code":   "FIELD_INVALID",
							"error":  "name " + detail,
							"params": map[string]any{"field": "name", "detail": detail},
						}
						if !reflect.DeepEqual(actual, want) {
							t.Errorf("invalid name wire=%s, want %#v", rec.Body, want)
						}
						after := call(http.MethodGet, "/api/v1/nodes", nil)
						if after.Code != http.StatusOK || after.Body.String() != before.Body.String() {
							t.Errorf("rejection changed node inventory: before=%s after=%s", before.Body, after.Body)
						}
						if !reflect.DeepEqual(installer.req, beforeInstaller) {
							t.Error("rejected name reached the SSH installer")
						}
						select {
						case event := <-sub.C():
							t.Errorf("rejected write published a node event: %+v", event)
						default:
						}
						return
					}
					wantStatus := http.StatusCreated
					if operation != "create" {
						wantStatus = http.StatusOK
					}
					if rec.Code != wantStatus {
						t.Fatalf("accepted name status=%d body=%s", rec.Code, rec.Body)
					}
					var node map[string]any
					if err := json.Unmarshal(rec.Body.Bytes(), &node); err != nil {
						t.Fatal(err)
					}
					if operation == "create" {
						if token, _ := node["token"].(string); token == "" {
							t.Error("created node has no connection token")
						}
						node = node["node"].(map[string]any)
						nodeUUID, _ = node["uuid"].(string)
					} else if operation == "install" {
						nodeUUID, _ = node["node_uuid"].(string)
						if installer.req.Target != "root@example.com" || installer.req.Token == "" {
							t.Error("accepted install did not reach the fixture with a node identity")
						}
					} else if _, present := node["token"]; present {
						t.Error("rename exposed node token")
					}
					if nodeUUID == "" || (operation != "install" && node["name"] != test.want) {
						t.Fatalf("accepted node name=%v uuid=%q", node["name"], nodeUUID)
					}
					read := call(http.MethodGet, "/api/v1/nodes/"+nodeUUID, nil)
					var saved map[string]any
					if err := json.Unmarshal(read.Body.Bytes(), &saved); err != nil {
						t.Fatal(err)
					}
					if read.Code != http.StatusOK || saved["name"] != test.want {
						t.Errorf("saved name=%v status=%d", saved["name"], read.Code)
					}
				})
			}
		})
	}
}
