package inferencecatalog

import (
	"strings"
	"testing"
)

// hygonPDSpecYAML is a PD-only spec in the shape of a hand-written GLM-5.3
// W4A8 launch on Hygon DCU: a prefill instance of two nodes at TP 8 x PP 2,
// a decode instance of two nodes at TP 16, and one script for every rank.
const hygonPDSpecYAML = `models:
- model_id: example/glm-w4a8
  source: user
  specs:
  - name: w4a8-sglang-hygon-pd
    engine: sglang
    image: registry.example:5000/sglang:0.5.18-glm53w4a8
    pd_mode: sglang-mooncake
    pd_roles:
      prefill: {tp: 8, pp: 2, nodes: 2}
      decode: {tp: 16, nodes: 2, units: 1}
    manual_command: |
      set -eu
      case "${GPUWORKS_PD_ROLE}" in
        prefill) exec python3 -m sglang.launch_server --node-rank ${GPUWORKS_NODE_RANK} --pp-size 2 ;;
        decode) exec python3 -m sglang.launch_server --node-rank ${GPUWORKS_NODE_RANK} --dp-size 16 ;;
      esac
    weights: {quantization: w4a8, sources: [{modelscope: example/glm-w4a8}]}
    gpus: {vendor: hygon}
`

func TestPDRolesAndManualCommandRoundTrip(t *testing.T) {
	catalog, err := ParseYAML([]byte(hygonPDSpecYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatal(err)
	}
	spec := catalog.Models[0].Specs[0]
	prefill, decode := spec.PDRoles["prefill"], spec.PDRoles["decode"]
	if prefill.TP != 8 || prefill.PipelineDegree() != 2 || prefill.NodeCount() != 2 || prefill.UnitCount() != 1 || prefill.GPUsPerNode() != 8 {
		t.Fatalf("prefill = %+v", prefill)
	}
	if decode.TP != 16 || decode.PipelineDegree() != 1 || decode.GPUsPerNode() != 8 {
		t.Fatalf("decode = %+v", decode)
	}
	if !strings.HasPrefix(spec.ManualCommand, "set -eu") || strings.HasSuffix(spec.ManualCommand, "\n") {
		t.Fatalf("manual_command = %q", spec.ManualCommand)
	}
	rows, err := ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	row := rows[0].Specs[0]
	if row.PDRoles != `{"decode":{"tp":16,"nodes":2,"units":1},"prefill":{"tp":8,"pp":2,"nodes":2}}` || row.ManualCommand != spec.ManualCommand {
		t.Fatalf("row pd_roles = %s manual_command = %q", row.PDRoles, row.ManualCommand)
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Specs[0]; got.PDRoles["prefill"] != prefill || got.PDRoles["decode"] != decode || got.ManualCommand != spec.ManualCommand {
		t.Fatalf("restored = %+v", got)
	}
	// A spec without either stores blanks, which read back as none.
	plain := flatFixture()
	if err := ValidateFlatCatalog(&plain); err != nil {
		t.Fatal(err)
	}
	plainRows, _ := ToFlatStoreModels(plain)
	if plainRows[0].Specs[0].PDRoles != "" || plainRows[0].Specs[0].ManualCommand != "" {
		t.Fatalf("plain row = %+v", plainRows[0].Specs[0])
	}
	if shapes, err := DecodePDRoleShapes(""); err != nil || shapes != nil {
		t.Fatalf("blank = %v %v", shapes, err)
	}
	if _, err := DecodePDRoleShapes("{"); err == nil {
		t.Fatal("a broken column decodes")
	}
}

// A manual command is any spec's to own, not only a PD spec's.
func TestManualCommandOnAnOrdinarySpec(t *testing.T) {
	catalog := flatFixture()
	catalog.Models[0].Specs[0].ManualCommand = "  exec vllm serve ${GPUWORKS_MODEL_PATH} --port ${GPUWORKS_API_PORT}\n"
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatal(err)
	}
	if got := catalog.Models[0].Specs[0].ManualCommand; got != "exec vllm serve ${GPUWORKS_MODEL_PATH} --port ${GPUWORKS_API_PORT}" {
		t.Fatalf("manual_command = %q", got)
	}
}

func TestPDShapeRefusals(t *testing.T) {
	for _, c := range []struct {
		name, from, to, want string
	}{
		{"layout without a mode", "    pd_mode: sglang-mooncake\n", "", "without a pd_mode"},
		{"role the mode lacks", "      decode: {tp: 16, nodes: 2, units: 1}", "      decode: {tp: 16, nodes: 2}\n      encode: {tp: 1}", `names "encode"`},
		{"missing role", "      decode: {tp: 16, nodes: 2, units: 1}\n", "", "has no decode role"},
		{"zero tp", "prefill: {tp: 8,", "prefill: {tp: 0,", "tp of at least 1"},
		{"negative units", "units: 1}", "units: -1}", "negative"},
		{"too many units", "units: 1}", "units: 33}", "more than 32"},
		{"uneven nodes", "prefill: {tp: 8, pp: 2, nodes: 2}", "prefill: {tp: 3, pp: 1, nodes: 2}", "does not divide evenly"},
		{"decode pipeline", "decode: {tp: 16, nodes: 2, units: 1}", "decode: {tp: 8, pp: 4, nodes: 2, units: 1}", "decode PP"},
		{"role-blind script", "case \"${GPUWORKS_PD_ROLE}\" in", "case \"$ROLE\" in", "${GPUWORKS_PD_ROLE}"},
		{"rank-blind script", "--node-rank ${GPUWORKS_NODE_RANK}", "--node-rank 0", "${GPUWORKS_NODE_RANK}"},
		{"argv instead of a script", "    manual_command: |\n      set -eu\n", "    manual_command: |\n      [\"python3\"]\n", "not a JSON argv"},
	} {
		doc := strings.Replace(hygonPDSpecYAML, c.from, c.to, -1)
		if doc == hygonPDSpecYAML {
			t.Fatalf("%s: the fixture has no %q", c.name, c.from)
		}
		catalog, err := ParseYAML([]byte(doc))
		if err == nil {
			err = ValidateFlatCatalog(&catalog)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	// A PD spec without a layout still has its script checked for the role.
	noLayout := strings.Replace(strings.Replace(hygonPDSpecYAML, "    pd_roles:\n      prefill: {tp: 8, pp: 2, nodes: 2}\n      decode: {tp: 16, nodes: 2, units: 1}\n", "", 1), "case \"${GPUWORKS_PD_ROLE}\" in", "case \"$ROLE\" in", 1)
	catalog, err := ParseYAML([]byte(noLayout))
	if err == nil {
		err = ValidateFlatCatalog(&catalog)
	}
	if err == nil || !strings.Contains(err.Error(), "${GPUWORKS_PD_ROLE}") {
		t.Fatalf("no layout: %v", err)
	}
}
