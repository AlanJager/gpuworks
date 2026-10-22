package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func previewTP(t *testing.T, launch deploy.ResolvedLaunch, name string, templateResidual, userResidual []string) deploy.ResolvedLaunch {
	t.Helper()
	engine, err := deploy.SelectEngine(name)
	if err != nil {
		t.Fatal(err)
	}
	return deploy.SymbolizePreviewTP(engine, launch, templateResidual, userResidual)
}

func TestSymbolizeDynamicTPRewritesUnclaimedTPFromArgv(t *testing.T) {
	result := deploy.ResolvedLaunch{
		Entrypoint: []string{"vllm", "serve", "/model"},
		Argv:       []string{"--host", "127.0.0.1", "--tensor-parallel-size", "1"},
	}
	out := previewTP(t, result, "vllm", nil, nil)
	if got := strings.Join(out.Argv, " "); !strings.Contains(got, "--tensor-parallel-size $TP_SIZE") || strings.Contains(got, "--tensor-parallel-size 1") {
		t.Fatalf("argv = %v", out.Argv)
	}
	if out.Entrypoint[0] != "vllm" || out.Entrypoint[2] != "/model" {
		t.Fatalf("entrypoint changed: %v", out.Entrypoint)
	}
}

func TestSymbolizeDynamicTPSGLangAliasFromArgv(t *testing.T) {
	result := deploy.ResolvedLaunch{
		Entrypoint: []string{"python", "-m", "sglang.launch_server", "--model-path", "/model", "--tp-size", "1"},
	}
	out := previewTP(t, result, "sglang", nil, nil)
	if got := strings.Join(out.Entrypoint, " "); !strings.Contains(got, "--tp-size $TP_SIZE") {
		t.Fatalf("entrypoint = %v", out.Entrypoint)
	}
}

func TestSymbolizeDynamicTPTemplateResidualClaimsFlag(t *testing.T) {
	result := deploy.ResolvedLaunch{
		Entrypoint: []string{"vllm", "serve", "/model"},
		Argv:       []string{"--tensor-parallel-size", "4"},
	}
	out := previewTP(t, result, "vllm", []string{"--tensor-parallel-size", "4"}, nil)
	if !reflect.DeepEqual(out, result) {
		t.Fatalf("template residual must keep the preview intact: %v", out)
	}
	// The equals spelling names the flag just the same.
	out = previewTP(t, result, "vllm", []string{"--tensor-parallel-size=8"}, nil)
	if !reflect.DeepEqual(out, result) {
		t.Fatalf("template residual in equals form must keep the preview intact: %v", out)
	}
}

func TestSymbolizeDynamicTPUserResidualAliasBlocksSGLang(t *testing.T) {
	result := deploy.ResolvedLaunch{
		Entrypoint: []string{"python", "-m", "sglang.launch_server", "--model-path", "/model", "--tp-size", "1"},
		Argv:       []string{"--tp", "2"},
	}
	out := previewTP(t, result, "sglang", nil, []string{"--tp", "2"})
	if !reflect.DeepEqual(out, result) {
		t.Fatalf("user --tp alias must keep the preview intact: %v", out)
	}
}

func TestSymbolizeDynamicTPUserResidualAliasBlocksVLLM(t *testing.T) {
	result := deploy.ResolvedLaunch{
		Entrypoint: []string{"vllm", "serve", "/model"},
		Argv:       []string{"--tensor-parallel-size", "1", "--tp", "2"},
	}
	out := previewTP(t, result, "vllm", nil, []string{"--tp", "2"})
	if !reflect.DeepEqual(out, result) {
		t.Fatalf("user --tp alias must keep the preview intact: %v", out)
	}
	out = previewTP(t, result, "vllm", []string{"--tp-size=2"}, nil)
	if !reflect.DeepEqual(out, result) {
		t.Fatalf("template --tp-size alias must keep the preview intact: %v", out)
	}
}

func TestSymbolizeDynamicTPNonOneTPStays(t *testing.T) {
	result := deploy.ResolvedLaunch{
		Entrypoint: []string{"vllm", "serve", "/model"},
		Argv:       []string{"--tensor-parallel-size", "4"},
	}
	out := previewTP(t, result, "vllm", nil, nil)
	if !reflect.DeepEqual(out, result) {
		t.Fatalf("a concrete scheduler-chosen TP must stay numeric: %v", out.Argv)
	}
}

// A manual-mode instance keeps the command it was created with: the instance
// view reads the desired-state snapshot, and the retired injected_args ledger
// must not reappear on the wire.
func TestInstanceDTOManualCommandFromDesiredState(t *testing.T) {
	st, err := store.Open("file:api_instance_manual_command?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("manual-command-node", store.HashToken("manual-command-token"), "manual-command-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "manual-command", NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	desired := proto.InstanceDesiredState{
		ResolvedLaunch: proto.ResolvedLaunch{
			CommandMode: string(deploy.CommandManual),
			Entrypoint:  []string{"/bin/sh"},
			Argv:        []string{"-c", "set -a\nvllm serve /model --port 30000"},
		},
		UUID:         "manual-command-instance",
		DeploymentID: dep.ID,
		DesiredState: "running",
	}
	raw, err := json.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := st.CreateInstance(&store.Instance{UUID: desired.UUID, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPending, DesiredStateJSON: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	dto := (&apiHandlers{d: Deps{Store: st}}).instanceDTO(instance)
	command := append(append([]string{}, dto.Entrypoint...), dto.Argv...)
	want := []string{"/bin/sh", "-c", "set -a\nvllm serve /model --port 30000"}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %#v, want %#v", command, want)
	}
	if dto.CommandMode != "manual" {
		t.Fatalf("command_mode = %q, want manual", dto.CommandMode)
	}
	wire, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "injected") {
		t.Fatalf("instance wire still carries the injected ledger: %s", wire)
	}
	if !strings.Contains(string(wire), `"command_mode":"manual"`) {
		t.Fatalf("instance wire lost command_mode: %s", wire)
	}
}
