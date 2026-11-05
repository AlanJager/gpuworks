package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/templateexport"
)

const catalogTemplateFixturesEnv = "GPUWORKS_UPDATE_UI_FIXTURES"

// The "Save as template" dialog's answers, from the real router, exporter and
// import: an export whose check passed and that left two env vars out, the
// same export keeping one of them, an export whose check failed, a refusal,
// and the import of the exported YAML.
//
// Regenerate: GPUWORKS_UPDATE_UI_FIXTURES=1 go test ./internal/server/app/ -run TestCatalogTemplateUIFixtures
func TestCatalogTemplateUIFixtures(t *testing.T) {
	const dir = "../../../ui/tests/fixtures/catalog-template"
	update := os.Getenv(catalogTemplateFixturesEnv) != ""
	f := newTemplateFixture(t)
	f.update(t, "container_overrides", `{"env":{"HF_TOKEN":"hf_secret","VLLM_LOGGING_LEVEL":"DEBUG"}}`)
	f.persistRuntimeLaunch(t)
	reply := templateexport.ProbeReply{HTTPStatus: http.StatusOK, InstanceUUID: f.instance.UUID}
	signer, token := adminSession(t, f.st)
	router := api.Router(api.Deps{Store: f.st, AuthSigner: signer, CatalogTemplates: catalogTemplates{store: f.st,
		probe: func(context.Context, uint, string, string, map[string]any) templateexport.ProbeReply { return reply }}})
	call := func(method, path, contentType, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	exportPath := fmt.Sprintf("/api/v1/deployments/%d/catalog-template", f.deployment.ID)

	status, exported := call(http.MethodPost, exportPath, "", "")
	if status != http.StatusOK {
		t.Fatalf("export = %d %s", status, exported)
	}
	checkCatalogTemplateFixture(t, dir, "exported", exported, update)
	if status, body := call(http.MethodPost, exportPath, "application/json", `{"include_env":["VLLM_LOGGING_LEVEL"]}`); status != http.StatusOK {
		t.Fatalf("export keeping env = %d %s", status, body)
	} else {
		checkCatalogTemplateFixture(t, dir, "exported-with-env", body, update)
	}

	var answer struct {
		YAML string `json:"yaml"`
	}
	if err := json.Unmarshal([]byte(exported), &answer); err != nil {
		t.Fatal(err)
	}
	if status, body := postYAML(router, token, "/api/v1/inference-templates/import", answer.YAML); status != http.StatusOK {
		t.Fatalf("import = %d %s", status, body)
	} else {
		checkCatalogTemplateFixture(t, dir, "imported", body, update)
	}

	reply = templateexport.ProbeReply{HTTPStatus: http.StatusInternalServerError, Message: "engine error", InstanceUUID: f.instance.UUID}
	if status, body := call(http.MethodPost, exportPath, "", ""); status != http.StatusOK {
		t.Fatalf("export with a failed check = %d %s", status, body)
	} else {
		checkCatalogTemplateFixture(t, dir, "check-failed", body, update)
	}

	f.update(t, "desired_state", store.DeployStopped)
	if status, body := call(http.MethodPost, exportPath, "", ""); status != http.StatusConflict {
		t.Fatalf("stopped = %d %s", status, body)
	} else {
		checkCatalogTemplateFixture(t, dir, "not-running", body, update)
	}
	if update {
		t.Fatalf("regenerated fixtures under %s; rerun without %s", dir, catalogTemplateFixturesEnv)
	}
}

func checkCatalogTemplateFixture(t *testing.T, dir, name, body string, update bool) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(body), "", "  "); err != nil {
		t.Fatal(err)
	}
	got := append(bytes.TrimRight(pretty.Bytes(), "\n"), '\n')
	path := filepath.Join(dir, name+".json")
	if update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture (regenerate with %s=1 go test ./internal/server/app/ -run %s): %v", catalogTemplateFixturesEnv, t.Name(), err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s drifted from the server's answer; regenerate with %s=1\ngot:\n%s", path, catalogTemplateFixturesEnv, got)
	}
}
