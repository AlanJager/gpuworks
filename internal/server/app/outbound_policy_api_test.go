package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

type outboundPolicyWire struct {
	Restrict bool     `json:"restrict_private_hosts"`
	Hosts    []string `json:"allow_private_hosts"`
	Editable bool     `json:"editable"`
	Revision string   `json:"revision"`
}

func decodeOutboundPolicyWire(t *testing.T, body []byte) outboundPolicyWire {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("policy JSON: %v: %s", err, body)
	}
	if len(keys) != 4 {
		t.Fatalf("policy wire keys=%v, want only restrict_private_hosts, allow_private_hosts, editable, revision", keys)
	}
	for _, key := range []string{"restrict_private_hosts", "allow_private_hosts", "editable", "revision"} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("policy omitted %s: %s", key, body)
		}
	}
	var result outboundPolicyWire
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Hosts == nil || !result.Editable || result.Revision == "" {
		t.Fatalf("file-backed policy has invalid wire values: %s", body)
	}
	return result
}

func assertOutboundPolicyError(t *testing.T, body []byte, code string) {
	t.Helper()
	var errorBody struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &errorBody); err != nil || errorBody.Code != code {
		t.Fatalf("error body=%s want code=%s err=%v", body, code, err)
	}
}

func TestRunningAppOutboundPolicyAPIControlsAndPersistsAccess(t *testing.T) {
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{
		RepoTags: []string{"example/policy-api:v1"}, LayerBytes: 256 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, archiveServer := ocifixture.StartArchiveServer(archive)
	t.Cleanup(archiveServer.Close)
	var hits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		archiveServer.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(origin.Close)
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	initial := "# operator note\nusage_report: false\nauto_check_updates: false\nlog:\n  keep: 7\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	application, base := startOutboundRuntimeApp(t, path)
	token := login(t, base)
	endpoint := base + "/api/v1/server/outbound-policy"
	code, body := status(t, http.MethodGet, endpoint, "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("anonymous policy GET=%d body=%s", code, body)
	}
	code, body = status(t, http.MethodPut, endpoint, "", []byte(`{}`))
	if code != http.StatusUnauthorized {
		t.Fatalf("anonymous policy PUT=%d body=%s", code, body)
	}
	code, body = status(t, http.MethodGet, endpoint, token, nil)
	if code != http.StatusOK {
		t.Fatalf("admin policy GET=%d body=%s", code, body)
	}
	current := decodeOutboundPolicyWire(t, body)
	if current.Restrict || len(current.Hosts) != 0 {
		t.Fatalf("default policy is not raw default-off configuration: %s", body)
	}
	var administrator store.User
	if err := application.store.DB().Where("username = ?", "admin").First(&administrator).Error; err != nil {
		t.Fatal(err)
	}
	viewer := store.User{Username: "policy-viewer", PasswordHash: administrator.PasswordHash, Role: "user"}
	if err := application.store.DB().Create(&viewer).Error; err != nil {
		t.Fatal(err)
	}
	code, body = status(t, http.MethodPost, base+"/api/v1/auth/login", "",
		[]byte(`{"username":"policy-viewer","password":"gpuworks-test-password-2026"}`))
	if code != http.StatusOK {
		t.Fatalf("viewer login=%d body=%s", code, body)
	}
	var viewerLogin struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &viewerLogin); err != nil || viewerLogin.Token == "" {
		t.Fatalf("viewer login token=%s err=%v", body, err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		code, body = status(t, method, endpoint, viewerLogin.Token, []byte(`{}`))
		if code != http.StatusForbidden {
			t.Fatalf("non-admin policy %s=%d body=%s", method, code, body)
		}
	}
	beforeInvalid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secretURL := "https://operator:private-password@source.invalid?token=private-token"
	invalidRequest, err := json.Marshal(map[string]any{
		"restrict_private_hosts": true, "allow_private_hosts": []string{secretURL}, "revision": current.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body = status(t, http.MethodPut, endpoint, token, invalidRequest)
	if code != http.StatusBadRequest {
		t.Fatalf("URL in private-host list accepted: status=%d body=%s", code, body)
	}
	assertOutboundPolicyError(t, body, "SERVER_OUTBOUND_POLICY_INVALID")
	for _, secret := range []string{secretURL, "private-password", "private-token"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("invalid policy response leaked credentials: %s", body)
		}
	}
	afterInvalid, err := os.ReadFile(path)
	if err != nil || string(afterInvalid) != string(beforeInvalid) {
		t.Fatalf("invalid policy save changed the file: %s err=%v", afterInvalid, err)
	}
	preview := func(want int) {
		t.Helper()
		before := hits.Load()
		code, body := status(t, http.MethodPost, base+"/api/v1/images/preview-archive-url", token,
			[]byte(fmt.Sprintf(`{"url":%q}`, origin.URL+"/image.tar")))
		if code != want {
			t.Fatalf("preview=%d want=%d body=%s", code, want, body)
		}
		if want == http.StatusOK {
			if hits.Load() <= before || !strings.Contains(string(body), `"repo_tags":["example/policy-api:v1"]`) {
				t.Fatalf("allowed preview did not read the real archive: %s", body)
			}
		} else {
			assertOutboundPolicyError(t, body, "IMAGE_ARCHIVE_URL_POLICY_DENIED")
			if hits.Load() != before {
				t.Fatal("policy-denied preview reached the origin")
			}
		}
	}
	put := func(restrict bool, hosts []string) outboundPolicyWire {
		t.Helper()
		request, err := json.Marshal(map[string]any{
			"restrict_private_hosts": restrict, "allow_private_hosts": hosts, "revision": current.Revision,
		})
		if err != nil {
			t.Fatal(err)
		}
		code, body := status(t, http.MethodPut, endpoint, token, request)
		if code != http.StatusOK {
			t.Fatalf("policy PUT=%d body=%s", code, body)
		}
		next := decodeOutboundPolicyWire(t, body)
		if next.Restrict != restrict || !slices.Equal(next.Hosts, hosts) || next.Revision == current.Revision {
			t.Fatalf("saved policy=%s did not match request=%s", body, request)
		}
		persisted, err := serverconfig.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.OutboundRestrictPrivateHosts != restrict || !slices.Equal(persisted.OutboundAllowPrivateHosts, hosts) {
			t.Fatalf("reloaded file policy=%+v, request=%s", persisted, request)
		}
		return next
	}
	preview(http.StatusOK)
	current = put(true, []string{})
	preview(http.StatusForbidden)
	current = put(true, []string{"127.0.0.1"})
	preview(http.StatusOK)
	current = put(false, []string{})
	preview(http.StatusOK)
	stored, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(stored), "# operator note") || !strings.Contains(string(stored), "keep: 7") {
		t.Fatalf("policy save lost unrelated configuration: %s err=%v", stored, err)
	}
	// An operator's file edit takes effect on this running app and makes an
	// already-open console's revision stale; that console cannot overwrite it.
	stale := current.Revision
	saveRuntimePrivateHosts(t, path, []string{})
	beforeConflict, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	request := []byte(fmt.Sprintf(`{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":%q}`, stale))
	code, body = status(t, http.MethodPut, endpoint, token, request)
	if code != http.StatusConflict {
		t.Fatalf("stale policy save=%d body=%s", code, body)
	}
	assertOutboundPolicyError(t, body, "SERVER_OUTBOUND_POLICY_CONFLICT")
	afterConflict, err := os.ReadFile(path)
	if err != nil || string(afterConflict) != string(beforeConflict) {
		t.Fatalf("stale policy save changed operator's file: %s err=%v", afterConflict, err)
	}
	preview(http.StatusForbidden)
	code, body = status(t, http.MethodGet, endpoint, token, nil)
	if code != http.StatusOK {
		t.Fatalf("reloaded policy GET=%d body=%s", code, body)
	}
	current = decodeOutboundPolicyWire(t, body)
	if !current.Restrict || len(current.Hosts) != 0 || current.Revision == stale {
		t.Fatalf("GET did not reflect the external file edit: %s", body)
	}
	current = put(false, []string{})
	preview(http.StatusOK)
}
