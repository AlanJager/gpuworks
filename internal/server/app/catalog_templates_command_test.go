package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/templateexport"
)

// exportScript is a manual command as the deploy form's editor writes it for
// the fixture's one-card vLLM launch.
const exportScript = "set -eu\nexport VLLM_LOGGING_LEVEL='DEBUG'\nexec vllm serve /model --served-model-name olmo --port 30000 --tensor-parallel-size 1"

// exportThroughAPI saves the fixture's deployment as a template through the
// router, the way the dialog does, after persisting the launch its current
// overrides give: the instance ran what the deployment states.
func exportThroughAPI(t *testing.T, f *templateFixture, overrides string) (int, map[string]any, string) {
	t.Helper()
	f.update(t, "container_overrides", overrides)
	f.persistRuntimeLaunch(t)
	signer, token := adminSession(t, f.st)
	router := api.Router(api.Deps{Store: f.st, AuthSigner: signer, CatalogTemplates: catalogTemplates{store: f.st,
		probe: func(context.Context, uint, string, string, map[string]any) templateexport.ProbeReply {
			return templateexport.ProbeReply{HTTPStatus: http.StatusOK, InstanceUUID: f.instance.UUID}
		}}})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/deployments/%d/catalog-template", f.deployment.ID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("answer %d is not JSON: %s", rec.Code, rec.Body.String())
	}
	return rec.Code, body, rec.Body.String()
}

func exportedSpec(t *testing.T, body map[string]any) inferencecatalog.InferenceSpec {
	t.Helper()
	catalog, err := inferencecatalog.ParseYAML([]byte(body["yaml"].(string)))
	if err != nil {
		t.Fatalf("exported YAML does not import: %v\n%s", err, body["yaml"])
	}
	return catalog.Models[0].Specs[0]
}

func wireNotes(body map[string]any) map[string]any {
	notes := map[string]any{}
	for _, raw := range body["notes"].([]any) {
		note := raw.(map[string]any)
		notes[note["code"].(string)] = note["params"]
	}
	return notes
}

func TestSaveAsTemplateWritesAManualScriptPinnedToTheTestedCards(t *testing.T) {
	f := newTemplateFixture(t)
	status, body, raw := exportThroughAPI(t, f, `{"command_mode":"manual","cmd_display":`+jsonString(exportScript)+`}`)
	if status != http.StatusOK || body["saveable"] != true || body["verified"] != true {
		t.Fatalf("export = %d %s", status, raw)
	}
	spec := exportedSpec(t, body)
	if spec.ManualCommand != exportScript || spec.Placement == nil || !strings.Contains(spec.Placement.Resolver, "gpu.count != 1 or gpu.nnodes != 1") {
		t.Fatalf("spec manual_command %q placement %+v", spec.ManualCommand, spec.Placement)
	}
	notes := wireNotes(body)
	if !reflect.DeepEqual(notes["cards_pinned"], map[string]any{"count": float64(1)}) {
		t.Fatalf("notes = %v, want cards_pinned {\"count\":1}", notes)
	}
	if _, ok := notes["manual_command"]; !ok {
		t.Fatalf("notes = %v, want manual_command", notes)
	}
	if _, ok := notes["placement_omitted"]; ok {
		t.Fatalf("notes = %v: a pinned placement is no omitted placement", notes)
	}
}

func TestSaveAsTemplateRedactsAManualScriptThatSetsASecret(t *testing.T) {
	f := newTemplateFixture(t)
	script := "export HF_TOKEN=hf_secret_value\n" + exportScript
	status, body, raw := exportThroughAPI(t, f, `{"env":{"HF_TOKEN":"hf_secret_value"},"command_mode":"manual","cmd_display":`+jsonString(script)+`}`)
	if status != http.StatusOK {
		t.Fatalf("export = %d %s", status, raw)
	}
	if strings.Contains(raw, "hf_secret_value") {
		t.Fatalf("the answer leaks the token: %s", raw)
	}
	if body["verified"] != true || body["saveable"] != false {
		t.Fatalf("verified %v saveable %v, want a verified export that cannot be saved as it is", body["verified"], body["saveable"])
	}
	if _, ok := wireNotes(body)["command_redacted"]; !ok {
		t.Fatalf("notes = %v, want command_redacted", body["notes"])
	}
}

func TestSaveAsTemplateNamesTheCommandsNoSpecFieldStates(t *testing.T) {
	cases := []struct{ name, overrides, reason string }{
		{"image entry", `{"command_mode":"image-entry"}`, "command_image_entry"},
		{"full argv", `{"cmd_display":"[\"python3\",\"-m\",\"vllm.entrypoints.openai.api_server\",\"--model\",\"/models\"]"}`, "command_full_argv"},
		{"manual script on a moved model", `{"command_mode":"manual","model_mount_path":"/data/olmo","cmd_display":` + jsonString(exportScript) + `}`, "command_model_mount"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTemplateFixture(t)
			status, body, raw := exportThroughAPI(t, f, tc.overrides)
			if status != http.StatusConflict || body["code"] != "TEMPLATE_EXPORT_UNAVAILABLE" {
				t.Fatalf("export = %d %s", status, raw)
			}
			if params := body["params"].(map[string]any); params["reason"] != tc.reason {
				t.Fatalf("params = %v, want reason %s", params, tc.reason)
			}
		})
	}
}

func TestSaveAsTemplateWritesAnEntrypointOverrideAndRefusesOneItCannotReproduce(t *testing.T) {
	t.Run("as written", func(t *testing.T) {
		status, body, raw := exportThroughAPI(t, newTemplateFixture(t), `{"entrypoint_display":"[\"python3\",\"-m\",\"vllm.entrypoints.openai.api_server\"]"}`)
		if status != http.StatusOK || body["saveable"] != true {
			t.Fatalf("export = %d %s", status, raw)
		}
		if spec := exportedSpec(t, body); !reflect.DeepEqual(spec.Entrypoint, []string{"python3", "-m", "vllm.entrypoints.openai.api_server"}) || spec.Placement != nil {
			t.Fatalf("spec entrypoint %q placement %+v", spec.Entrypoint, spec.Placement)
		}
	})
	// A deployment's own entrypoint is used as written; a spec's expands
	// launch variables. The same text would start something else.
	t.Run("not reproduced", func(t *testing.T) {
		status, body, raw := exportThroughAPI(t, newTemplateFixture(t), `{"entrypoint_display":"[\"python3\",\"/opt/${GPUWORKS_API_PORT}/serve.py\"]"}`)
		if status != http.StatusConflict || body["code"] != deploy.ErrorLaunchNotExportable || !reflect.DeepEqual(body["params"], map[string]any{"engine": "vllm"}) {
			t.Fatalf("export = %d %s, want 409 LAUNCH_NOT_EXPORTABLE", status, raw)
		}
	})
}

// A deployment made from a spec that owns its launch runs that spec's script
// as its manual command (deployment.applySpecManualCommand). That is the
// template's command, not the deployer's: it exports as the spec's own
// manual_command with the spec's placement and preparation.
func TestSaveAsTemplateExportsTheSourceSpecsOwnManualCommand(t *testing.T) {
	f := newTemplateFixture(t)
	resolver := "def resolve(gpu):\n    return {\"SP_DEGREE\": 1}\n"
	placement, err := json.Marshal(inferencecatalog.Placement{Language: "starlark", Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	f.installSnapshot(t, deploy.CatalogSelection{PreStartScript: "echo prepare", Placement: placement})
	mustCreateRow(t, f.st.DB().Model(&store.InferenceSpecRow{}).Where("name = ?", "runtime").Update("manual_command", exportScript).Error)

	status, body, raw := exportThroughAPI(t, f, `{"command_mode":"manual","cmd_display":`+jsonString(exportScript)+`}`)
	if status != http.StatusOK || body["saveable"] != true {
		t.Fatalf("export = %d %s", status, raw)
	}
	spec := exportedSpec(t, body)
	if spec.ManualCommand != exportScript || spec.Placement == nil || spec.Placement.Resolver != resolver || spec.PreStartScript != "echo prepare" {
		t.Fatalf("spec manual_command %q placement %+v pre_start %q, want the source spec's", spec.ManualCommand, spec.Placement, spec.PreStartScript)
	}
	if _, ok := wireNotes(body)["cards_pinned"]; ok {
		t.Fatalf("notes = %v: the source's own script keeps the source's placement", body["notes"])
	}

	// The deployer's own script on the same spec, owning its preparation.
	own := exportScript + " --seed 3"
	status, body, raw = exportThroughAPI(t, f, `{"command_mode":"manual","command_includes_preparation":true,"cmd_display":`+jsonString(own)+`}`)
	if status != http.StatusOK {
		t.Fatalf("export = %d %s", status, raw)
	}
	spec = exportedSpec(t, body)
	if spec.ManualCommand != own || spec.PreStartScript != "" || !strings.Contains(spec.Placement.Resolver, "return {\"SP_DEGREE\": 1}") {
		t.Fatalf("spec manual_command %q pre_start %q placement %+v", spec.ManualCommand, spec.PreStartScript, spec.Placement)
	}
	if !reflect.DeepEqual(wireNotes(body)["cards_pinned"], map[string]any{"count": float64(1)}) {
		t.Fatalf("notes = %v, want cards_pinned", body["notes"])
	}
}

func jsonString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// The script the editor seeds exports the deployment's own env, the fixture's
// HF_TOKEN included, which the export leaves out unless the user keeps it.
// Through the real router the value never reaches the YAML, the answer is not
// saveable, and the note names the key beside env_excluded: what the dialog
// says was left out stays left out.
func TestSaveAsTemplateWithholdsDeploymentEnvAScriptWritesOut(t *testing.T) {
	f := newTemplateFixture(t)
	script := "set -eu\nexport DASHSCOPE_KEY='dk-live-123'\n" + strings.TrimPrefix(exportScript, "set -eu\n")
	status, body, raw := exportThroughAPI(t, f, `{"env":{"DASHSCOPE_KEY":"dk-live-123"},"command_mode":"manual","cmd_display":`+jsonString(script)+`}`)
	if status != http.StatusOK {
		t.Fatalf("export = %d %s", status, raw)
	}
	if strings.Contains(raw, "dk-live-123") {
		t.Fatalf("the answer carries a withheld env value: %s", raw)
	}
	if body["saveable"] != false {
		t.Fatalf("saveable = %v, want false", body["saveable"])
	}
	notes := wireNotes(body)
	if !reflect.DeepEqual(notes["command_env_withheld"], map[string]any{"keys": []any{"DASHSCOPE_KEY"}}) {
		t.Fatalf("notes = %v, want command_env_withheld naming DASHSCOPE_KEY", body["notes"])
	}
	if _, ok := notes["env_excluded"]; !ok {
		t.Fatalf("notes = %v, want env_excluded too", body["notes"])
	}
}

// A script that reaches into an extra mount is refused: a deployment of the
// spec has no such mount.
func TestSaveAsTemplateRefusesAScriptThatUsesAnExtraMount(t *testing.T) {
	f := newTemplateFixture(t)
	script := "set -eu\npython /opt/patches/fix.py\n" + strings.TrimPrefix(exportScript, "set -eu\n")
	status, body, raw := exportThroughAPI(t, f, `{"extra_mounts":[{"host_path":"/srv/patches","container_path":"/opt/patches"}],"command_mode":"manual","cmd_display":`+jsonString(script)+`}`)
	if status != http.StatusConflict {
		t.Fatalf("export = %d %s", status, raw)
	}
	params, _ := body["params"].(map[string]any)
	if body["code"] != templateexport.ErrorUnavailable || params["reason"] != templateexport.ReasonCommandMount || params["mount_path"] != "/opt/patches" {
		t.Fatalf("refusal = %s", raw)
	}
}
