package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const templateDirsColumn = `[{"name":"pico-state","mount_path":"/pico","limit_bytes":429496729600,"retain":"deployment"}]`

func templateDirsStore(t *testing.T, name string) (*store.Store, *store.InferenceModel) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Dirs", Revision: "main", Name: "Acme/Dirs", WeightBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	spec := func(name, dirs string) store.InferenceSpecRow {
		return store.InferenceSpecRow{
			Name: name, Scenario: "balanced", Engine: "vllm", Quantization: "bf16", Image: "vllm/vllm-openai:latest", WeightBytes: 1 << 30, Dirs: dirs,
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Dirs"}]`, GPUs: `{"vendor":"nvidia"}`,
		}
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/dirs", DisplayTitle: "Dirs", Source: "upstream", Architecture: "qwen3", ContextLength: 4096,
		Specs: []store.InferenceSpecRow{spec("stateful", templateDirsColumn), spec("plain", "")},
	}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("acme/dirs")
	if err != nil {
		t.Fatal(err)
	}
	return st, template
}

// The template DTO states the directories a spec declares, in their own wire
// keys, and omits the key for a spec that declares none.
func TestInferenceTemplateViewStatesTheSpecsDirs(t *testing.T) {
	_, template := templateDirsStore(t, "template_view_dirs")
	view := inferenceTemplateView(*template)
	byName := map[string]json.RawMessage{}
	for _, spec := range view.Specs {
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		byName[spec.Name] = raw
	}
	if want := `"dirs":` + templateDirsColumn; !strings.Contains(string(byName["stateful"]), want) {
		t.Fatalf("stateful spec = %s, want %s", byName["stateful"], want)
	}
	if strings.Contains(string(byName["plain"]), `"dirs"`) {
		t.Fatalf("plain spec = %s, want no dirs key", byName["plain"])
	}
}

// The preview is the launch the instance will get: a declared directory shows
// as a managed mount at its container path with its ceiling, with no host path
// (the node picks it).
func TestEvaluatePreviewShowsTheTemplatesManagedDirectories(t *testing.T) {
	st, template := templateDirsStore(t, "evaluate_template_dirs")
	h := evaluateFieldErrorFixture(t, st)
	for name, test := range map[string]struct {
		spec string
		want bool
	}{"declared": {"stateful", true}, "none": {"plain", false}} {
		t.Run(name, func(t *testing.T) {
			status, raw := evaluateForFieldErrors(t, h, fmt.Sprintf(`{"template_id":%d,"template_spec_name":%q,"node_uuid":"field-errors-node"}`, template.ID, test.spec))
			if status != http.StatusOK || string(raw["ok"]) != "true" {
				t.Fatalf("status = %d body = %s", status, raw)
			}
			var mounts []map[string]any
			_ = json.Unmarshal(raw["mounts"], &mounts)
			found := false
			for _, mount := range mounts {
				if mount["purpose"] == "managed-dir" {
					found = true
					if mount["target"] != "/pico" || mount["size_hint"] != "429496729600" || mount["source"] != nil {
						t.Fatalf("managed mount = %v", mount)
					}
				}
			}
			if found != test.want {
				t.Fatalf("mounts = %s, want managed directory %v", raw["mounts"], test.want)
			}
		})
	}
}
