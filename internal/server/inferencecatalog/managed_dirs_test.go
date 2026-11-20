package inferencecatalog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

const managedDirsSpecYAML = `models:
- model_id: Org/Model
  specs:
  - name: tp4
    engine: vllm
    image: vllm/vllm-openai:v0.25.1
    weights: {quantization: bf16, sources: [{huggingface: Org/Model}]}
    gpus: {vendor: nvidia}
    dirs:
    - {name: pico-state, mount_path: /pico, limit_bytes: 429496729600}
    - {name: logs, mount_path: /var/log/pico, limit_bytes: 2147483648, retain: deployment}
`

// dirs is read from the catalog document, gets its retention written back,
// survives the store projection and comes back out of it, so the editor's
// YAML round-trips it.
func TestSpecDirsParseNormalizeAndRoundTripThroughTheStoreRows(t *testing.T) {
	catalog, err := ParseYAML([]byte(managedDirsSpecYAML))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	want := []deploy.ManagedDir{
		{Name: "pico-state", MountPath: "/pico", LimitBytes: 400 << 30, Retain: deploy.ManagedDirRetainDeployment},
		{Name: "logs", MountPath: "/var/log/pico", LimitBytes: 2 << 30, Retain: deploy.ManagedDirRetainDeployment},
	}
	var stored []deploy.ManagedDir
	if err := json.Unmarshal([]byte(rows[0].Specs[0].Dirs), &stored); err != nil || !reflect.DeepEqual(stored, want) {
		t.Fatalf("stored dirs = %s (%v)", rows[0].Specs[0].Dirs, err)
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil || !reflect.DeepEqual(restored.Specs[0].Dirs, want) {
		t.Fatalf("restored = %+v, %v", restored.Specs[0].Dirs, err)
	}

	// A spec that declares none stores the blank column and reads back none.
	rows, err = ToFlatStoreModels(flatFixture())
	if err != nil || rows[0].Specs[0].Dirs != "" {
		t.Fatalf("plain stored dirs = %q, %v", rows[0].Specs[0].Dirs, err)
	}
	plain, err := FromFlatStoreModel(rows[0])
	if err != nil || plain.Specs[0].Dirs != nil {
		t.Fatalf("plain restored = %+v, %v", plain.Specs[0].Dirs, err)
	}
}

// A directory may not land on a container path the spec already owns, and the
// declaration's own rules are enforced at import.
func TestSpecDirsMayNotOverlapTheSpecsOwnContainerPaths(t *testing.T) {
	for name, test := range map[string]struct {
		dirs string
		want string
	}{
		"model mount":       {"- {name: state, mount_path: /model/state, limit_bytes: 1}", "overlaps the model mount"},
		"parent of mount":   {"- {name: state, mount_path: /mnt, limit_bytes: 1}", "overlaps component draft mount_path"},
		"component mount":   {"- {name: state, mount_path: /mnt/draft, limit_bytes: 1}", "overlaps component draft mount_path"},
		"asset unpack":      {"- {name: state, mount_path: /opt/plugin, limit_bytes: 1}", "overlaps asset"},
		"declaration rules": {"- {name: State, mount_path: /state, limit_bytes: 1}", "name"},
		"unknown field":     {"- {name: state, mount_path: /state, limit_bytes: 1, path: /x}", "path"},
	} {
		t.Run(name, func(t *testing.T) {
			document := strings.Replace(componentAssetSpecYAML, "    __DIRS__", "    dirs:\n    "+test.dirs, 1)
			catalog, err := ParseYAML([]byte(document))
			if err == nil {
				err = ValidateFlatCatalog(&catalog)
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, test.want)
			}
		})
	}
	// The same document with a directory of its own imports.
	document := strings.Replace(componentAssetSpecYAML, "    __DIRS__", "    dirs:\n    - {name: state, mount_path: /state, limit_bytes: 1}", 1)
	catalog, err := ParseYAML([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatal(err)
	}
}

// componentAssetSpecYAML is a spec owning every kind of container path a
// directory could collide with: the primary mount, a component mount and an
// asset's unpack directory.
const componentAssetSpecYAML = `models:
- model_id: Org/Draft
  specs: []
- model_id: Org/Model
  specs:
  - name: tp4
    engine: vllm
    image: vllm/vllm-openai:v0.25.1
    weights: {quantization: bf16, sources: [{huggingface: Org/Model}]}
    gpus: {vendor: nvidia}
    args: [--speculative-config, '{"model": "/mnt/draft"}']
    assets:
    - {file: assets/plugin.py, sha256: 0000000000000000000000000000000000000000000000000000000000000000, unpack_to: /opt/plugin}
    components:
    - {name: main, role: primary, weights: {quantization: bf16, sources: [{huggingface: Org/Model}]}, weight_bytes: 1, mount_path: /model}
    - {name: draft, role: speculative_draft, model_id: Org/Draft, weights: {quantization: bf16, sources: [{huggingface: Org/Draft}]}, weight_bytes: 1, mount_path: /mnt/draft}
    __DIRS__
`

func TestSchemaDescribesSpecDirs(t *testing.T) {
	raw, err := json.Marshal(JSONSchema())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"dirs":{`, `"limit_bytes":{`, `"retain":{`, `"enum":["deployment"]`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("schema lacks %s", want)
		}
	}
}
