package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func checkHistoricalBatchKeyIsolation(t *testing.T, env *testenv.Env, admin *testenv.APIClient, input string) {
	t.Helper()
	// These rows model existing keys whose creator cannot be recovered. New
	// keys must go through HTTP instead; assigning them to user zero again
	// would hide whether the creation fix works.
	legacy, err := env.Store().CreateAPIKey("historical-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.Store().CreateAPIKey("historical-b", 0)
	if err != nil {
		t.Fatal(err)
	}
	client := &testenv.APIClient{Client: admin.Client, BaseURL: admin.BaseURL, Token: legacy}
	raw := ownershipUpload(t, client, "/v1/files", input)
	var file struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &file) != nil || file.ID == "" {
		t.Fatalf("legacy upload=%s", raw)
	}
	resp, err := client.PostJSON("/v1/batches", []byte(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, file.ID)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var b struct {
		ID string `json:"id"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &b) != nil || b.ID == "" {
		t.Fatalf("legacy batch=%d %s", resp.StatusCode, raw)
	}
	for _, path := range []string{"/v1/batches/" + b.ID, "/v1/files/" + file.ID + "/content"} {
		if code, raw := ownershipGet(t, client, path); code != 200 {
			t.Errorf("original legacy key GET=%d %s", code, raw)
		}
	}
	for _, token := range []string{other, ""} {
		stranger := &testenv.APIClient{Client: admin.Client, BaseURL: admin.BaseURL, Token: token}
		for _, path := range []string{"/v1/batches/" + b.ID, "/v1/files/" + file.ID, "/v1/files/" + file.ID + "/content"} {
			if code, raw := ownershipGet(t, stranger, path); code != 404 {
				t.Errorf("legacy stranger GET=%d %s", code, raw)
			}
		}
		for _, path := range []string{"/v1/batches", "/v1/files"} {
			code, raw := ownershipGet(t, stranger, path)
			if code != 200 || bytes.Contains(raw, []byte(b.ID)) || bytes.Contains(raw, []byte(file.ID)) {
				t.Errorf("legacy stranger listing=%d %s", code, raw)
			}
		}
		resp, err := stranger.PostJSON("/v1/batches/"+b.ID+"/cancel", []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("legacy stranger cancel=%d %s", resp.StatusCode, raw)
		}
		resp, err = stranger.Delete("/v1/files/" + file.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("legacy stranger delete=%d %s", resp.StatusCode, raw)
		}
		resp, err = stranger.PostJSON("/v1/batches", []byte(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, file.ID)))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || !bytes.Contains(raw, []byte("file_not_found")) {
			t.Errorf("legacy stranger reuses input=%d %s", resp.StatusCode, raw)
		}
	}
	// Explicitly replacing a key does not silently transfer the original
	// key's old rows, but an administrator can still manage them in console.
	replacement := ownershipHTTPKey(t, admin, `{"name":"explicit-personal-replacement"}`)
	personal := &testenv.APIClient{Client: admin.Client, BaseURL: admin.BaseURL, Token: replacement}
	if code, raw := ownershipGet(t, personal, "/v1/batches/"+b.ID); code != 404 {
		t.Errorf("reissue inherited unassigned history=%d %s", code, raw)
	}
	if code, raw := ownershipGet(t, admin, "/api/v1/batches/"+b.ID); code != 200 {
		t.Errorf("admin lost historical batch=%d %s", code, raw)
	}
	resp, err = admin.Delete("/api/v1/apikeys/" + auth.PrefixOf(legacy))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy revoke=%d", resp.StatusCode)
	}
	if code, raw := ownershipGet(t, client, "/v1/batches/"+b.ID); code != 401 {
		t.Errorf("revoked historical key authenticates=%d %s", code, raw)
	}
	if _, ok := env.Store().ResolveAPIKey(replacement); !ok {
		t.Error("revoking historical key invalidated personal replacement")
	}
}
