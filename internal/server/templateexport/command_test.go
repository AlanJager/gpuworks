package templateexport

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

// The script the deploy form's manual editor writes for a single-card vLLM
// launch (ui launchCommand): set -eu, an export line per launch env key with
// its value single-quoted, then exec and the command with the model mount, the
// served name, the port and the TP size written out.
const editorScript = "set -eu\nexport VLLM_LOGGING_LEVEL='DEBUG'\nexec vllm serve /model --served-model-name olmo --port 30000 --tensor-parallel-size 1 --max-model-len 4096"

func TestCommandOfSaysWhichCommandsASpecCanState(t *testing.T) {
	cases := []struct {
		name      string
		overrides deploy.ContainerOverrides
		want      Command
		reason    string
	}{
		{"engine command", deploy.ContainerOverrides{}, Command{}, ""},
		{"engine mode named", deploy.ContainerOverrides{CommandMode: "engine"}, Command{}, ""},
		{"entrypoint", deploy.ContainerOverrides{EntrypointDisplay: `["python3","-m","vllm.entrypoints.openai.api_server"]`},
			Command{Entrypoint: []string{"python3", "-m", "vllm.entrypoints.openai.api_server"}}, ""},
		{"manual script", deploy.ContainerOverrides{CommandMode: "manual", CmdDisplay: "  " + editorScript + "\n"},
			Command{ManualCommand: editorScript}, ""},
		{"manual script with preparation", deploy.ContainerOverrides{CommandMode: "manual", CmdDisplay: editorScript, CommandIncludesPreparation: true},
			Command{ManualCommand: editorScript, IncludesPreparation: true}, ""},
		{"engine full argv", deploy.ContainerOverrides{CmdDisplay: `["python","serve.py"]`}, Command{}, ReasonCommandFullArgv},
		{"manual argv", deploy.ContainerOverrides{CommandMode: "manual", CmdDisplay: `["python","serve.py"]`}, Command{}, ReasonCommandFullArgv},
		{"image entry", deploy.ContainerOverrides{CommandMode: "image-entry"}, Command{}, ReasonCommandImageEntry},
		{"manual without a command", deploy.ContainerOverrides{CommandMode: "manual"}, Command{}, ReasonCommandImageEntry},
		{"manual script on a moved model", deploy.ContainerOverrides{CommandMode: "manual", CmdDisplay: editorScript, ModelMountPath: "/data/model"}, Command{}, ReasonCommandModelMount},
		{"manual script naming the default mount", deploy.ContainerOverrides{CommandMode: "manual", CmdDisplay: editorScript, ModelMountPath: "/model/"},
			Command{ManualCommand: editorScript}, ""},
		{"manual script reaching into an extra mount", deploy.ContainerOverrides{CommandMode: "manual", CmdDisplay: "python /opt/patches/fix.py && " + editorScript,
			ExtraMounts: []deploy.ContainerMount{{HostPath: "/srv/patches", ContainerPath: "/opt/patches/"}}}, Command{}, ReasonCommandMount},
		{"manual script beside an unused mount", deploy.ContainerOverrides{CommandMode: "manual", CmdDisplay: editorScript,
			ExtraMounts: []deploy.ContainerMount{{HostPath: "/srv/cache", ContainerPath: "/root/.cache"}}}, Command{ManualCommand: editorScript}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CommandOf(tc.overrides)
			if tc.reason != "" {
				var product *messages.ProductError
				if !errors.As(err, &product) || product.Code() != ErrorUnavailable || product.Params()["reason"] != tc.reason {
					t.Fatalf("error = %v, want %s/%s", err, ErrorUnavailable, tc.reason)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CommandOf = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestReplayStartsTheSpecsCommandInsteadOfTheDeploymentsOwn(t *testing.T) {
	overrides := deploy.ContainerOverrides{CommandMode: "manual", CmdDisplay: "old", CommandIncludesPreparation: true, EntrypointDisplay: "x", Env: map[string]string{"K": "V"}}
	selection := deploy.CatalogSelection{PreStartScript: "prepare", PreStartArgv: []string{"a"}, Entrypoint: []string{"template"}}

	gotOverrides, gotSelection := Command{ManualCommand: editorScript, IncludesPreparation: true}.Replay(overrides, selection)
	if gotOverrides.CommandMode != "manual" || gotOverrides.CmdDisplay != editorScript || gotOverrides.CommandIncludesPreparation || gotOverrides.EntrypointDisplay != "" || gotOverrides.Env["K"] != "V" {
		t.Fatalf("overrides = %+v, want the spec's script applied as the create path does, nothing else changed", gotOverrides)
	}
	if gotSelection.PreStartScript != "" || gotSelection.PreStartArgv != nil || !slices.Equal(gotSelection.Entrypoint, []string{"template"}) {
		t.Fatalf("selection = %+v, want the pre-start steps the script owns dropped", gotSelection)
	}

	gotOverrides, gotSelection = Command{Entrypoint: []string{"python3"}}.Replay(deploy.ContainerOverrides{EntrypointDisplay: "python3"}, selection)
	if gotOverrides.EntrypointDisplay != "" || gotOverrides.CommandMode != "" || !slices.Equal(gotSelection.Entrypoint, []string{"python3"}) || gotSelection.PreStartScript != "prepare" {
		t.Fatalf("entrypoint replay = %+v %+v", gotOverrides, gotSelection)
	}
}

func TestBuildWritesAManualScriptPinnedToTheTestedCards(t *testing.T) {
	in := customInput()
	// The script exports the deployment's own VLLM_LOGGING_LEVEL, which the
	// user keeps in the template; otherwise it is withheld (below).
	in.IncludeEnv = []string{"VLLM_LOGGING_LEVEL"}
	in.GPUs = append(in.GPUs, in.GPUs[0])
	in.GPUCount = 2
	in.Command = Command{ManualCommand: editorScript}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	spec := out.Model.Specs[0]
	if spec.ManualCommand != editorScript || !out.Saveable {
		t.Fatalf("manual_command %q saveable %v", spec.ManualCommand, out.Saveable)
	}
	if spec.Placement == nil {
		t.Fatal("no placement: a script that hard-codes its parallelism must not be scheduled on another card count")
	}
	for _, shape := range []struct {
		count, nnodes int
		accepted      bool
	}{{2, 1, true}, {1, 1, false}, {4, 1, false}, {1, 2, false}} {
		result, err := placementeval.Resolve(context.Background(), spec.Placement.Resolver, placementeval.GPU{Count: shape.count, NNodes: shape.nnodes, Vendor: "nvidia"})
		if err != nil || (result.Reason == "") != shape.accepted {
			t.Fatalf("%d card(s) on %d node(s): %+v %v, accepted want %v", shape.count, shape.nnodes, result, err, shape.accepted)
		}
	}
	if note := findNote(out.Notes, NoteCardsPinned); note == nil || note.Params["count"] != 2 {
		t.Fatalf("notes = %+v, want cards_pinned count 2", out.Notes)
	}
	if !hasNote(out.Notes, NoteManualCommand) || hasNote(out.Notes, NotePlacementOmitted) || hasNote(out.Notes, NoteManualCommandPortUnstated) {
		t.Fatalf("notes = %+v", out.Notes)
	}
	imported, err := inferencecatalog.ParseYAML([]byte(out.YAML))
	if err != nil {
		t.Fatalf("yaml does not import: %v", err)
	}
	if got := imported.Models[0].Specs[0].ManualCommand; got != editorScript {
		t.Fatalf("imported manual_command = %q", got)
	}
}

func TestBuildRedactsAManualScriptThatSetsASecret(t *testing.T) {
	in := customInput()
	in.Command = Command{ManualCommand: "export HF_TOKEN=hf_secret\n" + editorScript}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.YAML, "hf_secret") || strings.Contains(out.Evidence, "hf_secret") || strings.Contains(out.Model.Specs[0].ManualCommand, "hf_secret") {
		t.Fatalf("the export leaks the token:\n%s", out.YAML)
	}
	if !out.Verified || out.Saveable || !hasNote(out.Notes, NoteCommandRedacted) {
		t.Fatalf("verified %v saveable %v notes %+v, want a verified export that cannot be saved as it is", out.Verified, out.Saveable, out.Notes)
	}
}

func TestBuildKeepsTheSourceSpecsOwnManualCommandAndPlacement(t *testing.T) {
	resolver := "def resolve(gpu):\n    if gpu.count != 4: return reject(\"needs 4\")\n    return {\"TP_SIZE\": 4, \"SP_DEGREE\": 1}\n"
	template := inferencecatalog.InferenceModel{ModelID: "allenai/olmo-2-0425-1b-instruct", Source: "upstream", DisplayTitle: "OLMo"}
	source := inferencecatalog.InferenceSpec{
		Name: "recipe", Engine: "vllm", Image: "old:image",
		Weights:        inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "allenai/OLMo-2-0425-1B-Instruct", Revision: commit}}},
		GPUs:           inferencecatalog.GPUSelector{Vendor: "nvidia"},
		PreStartScript: "pip install extra",
		Placement:      &inferencecatalog.Placement{Language: "starlark", Resolver: resolver},
	}
	template.Specs = []inferencecatalog.InferenceSpec{source}
	in := customInput()
	in.Template, in.TemplateSpec = &template, &template.Specs[0]
	in.UserArgs, in.Quick = nil, deploy.LaunchConfig{}
	script := "exec vllm serve ${GPUWORKS_MODEL_PATH} --port ${GPUWORKS_API_PORT} -tp ${GPUWORKS_TP_SIZE}"
	in.Command, in.SourceManualCommand = Command{ManualCommand: script}, script

	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	spec := out.Model.Specs[0]
	if spec.ManualCommand != script || spec.Placement == nil || spec.Placement.Resolver != resolver || spec.PreStartScript != "pip install extra" || hasNote(out.Notes, NoteCardsPinned) {
		t.Fatalf("spec = %+v notes %+v, want the source's script, placement and preparation as they were", spec, out.Notes)
	}

	// The deployer's own script on the same spec, owning its preparation:
	// the source's placement is kept behind the tested card count.
	in.Command = Command{ManualCommand: script + " --seed 1", IncludesPreparation: true}
	in.GPUs = []GPUFact{in.GPUs[0], in.GPUs[0], in.GPUs[0], in.GPUs[0]}
	in.GPUCount = 4
	if out, err = Build(in); err != nil {
		t.Fatal(err)
	}
	spec = out.Model.Specs[0]
	if spec.PreStartScript != "" || spec.PreStartArgv != nil || !hasNote(out.Notes, NoteCardsPinned) {
		t.Fatalf("spec = %+v notes %+v, want the script's own preparation and pinned cards", spec, out.Notes)
	}
	result, err := placementeval.Resolve(context.Background(), spec.Placement.Resolver, placementeval.GPU{Count: 4, NNodes: 1})
	if err != nil || result.Reason != "" || !reflect.DeepEqual(result.Variables, map[string]any{"TP_SIZE": int64(4), "SP_DEGREE": int64(1)}) {
		t.Fatalf("pinned resolver on the tested cards = %+v %v, want the source resolver's variables", result, err)
	}
	if _, err := inferencecatalog.ParseYAML([]byte(out.YAML)); err != nil {
		t.Fatalf("yaml does not import: %v", err)
	}
}

func TestBuildWritesAnEntrypointOverride(t *testing.T) {
	in := customInput()
	in.Command = Command{Entrypoint: []string{"python3", "-m", "vllm.entrypoints.openai.api_server"}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	spec := out.Model.Specs[0]
	if !slices.Equal(spec.Entrypoint, in.Command.Entrypoint) || spec.Placement != nil || spec.ManualCommand != "" || !hasNote(out.Notes, NotePlacementOmitted) {
		t.Fatalf("spec = %+v notes %+v, want the entrypoint and the args, scheduling free", spec, out.Notes)
	}
}

func TestBuildRefusesACommandTheImportWouldRefuse(t *testing.T) {
	cases := map[string]func(*Input){
		"manual command over the size limit": func(in *Input) {
			in.Command = Command{ManualCommand: "exec vllm serve " + strings.Repeat("x", inferencecatalog.MaxManualCommandBytes)}
		},
		"a resolver written on one line": func(in *Input) {
			model := inferencecatalog.InferenceModel{ModelID: "x/y", Source: "upstream"}
			spec := inferencecatalog.InferenceSpec{Name: "s", Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "x/y", Revision: commit}}},
				Placement: &inferencecatalog.Placement{Language: "starlark", Resolver: "def resolve(gpu): return {}\n"}}
			in.Template, in.TemplateSpec = &model, &spec
			in.Command = Command{ManualCommand: editorScript}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := customInput()
			mutate(&in)
			_, err := Build(in)
			var product *messages.ProductError
			if !errors.As(err, &product) || product.Params()["reason"] != ReasonCommandNotImportable {
				t.Fatalf("error = %v, want %s", err, ReasonCommandNotImportable)
			}
		})
	}
}

func TestBuildNotesAManualScriptThatStatesNoPort(t *testing.T) {
	in := customInput()
	in.Command = Command{ManualCommand: "exec vllm serve /models"}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	note := findNote(out.Notes, NoteManualCommandPortUnstated)
	if note == nil || !reflect.DeepEqual(note.Params, map[string]any{"flag": "--port", "variable": "${GPUWORKS_API_PORT}"}) || !out.Saveable {
		t.Fatalf("notes %+v saveable %v", out.Notes, out.Saveable)
	}
	if !strings.Contains(out.YAML, "# The manual command states no --port and does not reference ${GPUWORKS_API_PORT}") {
		t.Fatalf("yaml comments:\n%s", out.YAML)
	}
}

// The editor writes an export line for every launch env key, the deployment's
// own included. A key the export leaves out (it is not in IncludeEnv) must not
// reach the catalog through the script: its value is redacted wherever the
// command writes it, the result is not saveable, and a note names the key, so
// the env_excluded note stays true. A key the user chose to include is
// exported in env and left in the script.
func TestBuildWithholdsTheDeploymentEnvTheExportLeavesOut(t *testing.T) {
	in := customInput()
	in.UserEnv = map[string]string{"DASHSCOPE_KEY": "dk-live-123", "S3_CREDENTIALS": "abcd:efgh", "LOG_FORMAT": "json"}
	in.IncludeEnv = []string{"LOG_FORMAT"}
	script := "set -eu\nexport DASHSCOPE_KEY='dk-live-123'\nexport LOG_FORMAT='json'\n" +
		"exec vllm serve /model --served-model-name olmo --port 30000 --tensor-parallel-size 1 --s3-auth abcd:efgh"
	in.Command = Command{ManualCommand: script}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"dk-live-123", "abcd:efgh"} {
		if strings.Contains(out.YAML, leaked) || strings.Contains(out.Evidence, leaked) {
			t.Fatalf("the export carries %q though its env is left out:\n%s", leaked, out.YAML)
		}
	}
	if out.Saveable || !hasNote(out.Notes, NoteCommandRedacted) {
		t.Fatalf("saveable %v notes %+v; want an export that cannot be saved as it is", out.Saveable, out.Notes)
	}
	if got := noteParam(out.Notes, NoteCommandEnvWithheld, "keys"); !reflect.DeepEqual(got, []string{"DASHSCOPE_KEY", "S3_CREDENTIALS"}) {
		t.Fatalf("withheld keys = %v", got)
	}
	if !strings.Contains(out.Model.Specs[0].ManualCommand, "export LOG_FORMAT='json'") || out.Model.Specs[0].Env["LOG_FORMAT"] != "json" {
		t.Fatalf("an included key was withheld: %q env %v", out.Model.Specs[0].ManualCommand, out.Model.Specs[0].Env)
	}
	// With every key included nothing is withheld.
	in.IncludeEnv = []string{"DASHSCOPE_KEY", "S3_CREDENTIALS", "LOG_FORMAT"}
	if out, err = Build(in); err != nil || !out.Saveable || hasNote(out.Notes, NoteCommandEnvWithheld) {
		t.Fatalf("all env included: saveable %v notes %+v err %v", out.Saveable, out.Notes, err)
	}
}

// An entrypoint that carries a secret is redacted like a manual script.
func TestBuildRedactsAnEntrypointThatCarriesASecret(t *testing.T) {
	in := customInput()
	in.Command = Command{Entrypoint: []string{"env", "HF_TOKEN=hf_abcdefghijklmnopqrstuvwxyz", "python3", "-m", "vllm.entrypoints.openai.api_server"}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.YAML, "hf_abcdefghijklmnopqrstuvwxyz") || out.Saveable || !hasNote(out.Notes, NoteCommandRedacted) {
		t.Fatalf("saveable %v notes %+v yaml:\n%s", out.Saveable, out.Notes, out.YAML)
	}
}

// A script that sets an env key the platform sets for each node (here the
// collective interface) fixes this node's value: the export says so.
func TestBuildNotesAScriptThatSetsPlatformEnv(t *testing.T) {
	in := customInput()
	in.IncludeEnv = []string{"VLLM_LOGGING_LEVEL"}
	in.PlatformEnv = map[string]string{"NCCL_SOCKET_IFNAME": "eth0", "GPUWORKS_API_PORT": "30000"}
	in.Command = Command{ManualCommand: "set -eu\nexport NCCL_SOCKET_IFNAME='eth0'\n" + strings.TrimPrefix(editorScript, "set -eu\n")}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := noteParam(out.Notes, NoteCommandSetsPlatformEnv, "keys"); !reflect.DeepEqual(got, []string{"NCCL_SOCKET_IFNAME"}) {
		t.Fatalf("platform env keys = %v, notes %+v", got, out.Notes)
	}
	if !out.Saveable {
		t.Fatal("a note about platform env made the export unsaveable")
	}
}

func noteParam(notes []Note, code, key string) any {
	for _, n := range notes {
		if n.Code == code {
			return n.Params[key]
		}
	}
	return nil
}
