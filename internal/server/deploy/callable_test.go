package deploy

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A callable deployment owns its command line: the catalog's Serve Adapter
// reads the template's arguments itself. Materializing must therefore leave
// every one of them in the residual the adapter receives - absorbing them into
// dictionary fields would silently drop them, because that adapter never reads
// a field.
func TestMaterializeCallableKeepsAllTemplateArgs(t *testing.T) {
	templateArgs := []string{"--dtype", "bfloat16", "--max-model-len", "4096", "--adapter", "funasr"}
	out, err := Materialize(MaterializeInput{Engine: EngineCallable, TemplateArgs: templateArgs})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.TemplateResidual, templateArgs) {
		t.Fatalf("template residual = %v, want every template argument kept: %v", out.TemplateResidual, templateArgs)
	}
	if len(out.TemplateParams) != 0 || len(out.Moved) != 0 {
		t.Fatalf("callable must not absorb dictionary parameters into fields: %v moved=%v", out.TemplateParams, out.Moved)
	}
	engine, err := SelectEngine(EngineCallable)
	if err != nil {
		t.Fatal(err)
	}
	built := engine.Build(Params{ModelPath: "/model", Host: "0.0.0.0", APIPort: 19001, TemplateArgs: out.TemplateResidual})
	for _, want := range templateArgs {
		if !slices.Contains(built.Argv, want) {
			t.Fatalf("template argument %q is missing from the command: %v", want, built.Argv)
		}
	}
}

// callableServeArgs is how a catalog callable spec places the platform facts
// its adapter needs: the engine appends none, so the template names them.
var callableServeArgs = []string{"--model", "${MODEL_PATH}", "--host", "${API_HOST}", "--port", "${API_PORT}"}

func callableLaunch(t *testing.T, params Params, input ResolveLaunchInput) (ResolvedLaunch, error) {
	t.Helper()
	engine, err := SelectEngine(EngineCallable)
	if err != nil {
		t.Fatal(err)
	}
	input.Engine, input.Params = engine, params
	return ResolveLaunch(input)
}

// The adapter's argv is exactly what the template wrote, with the platform's
// facts expanded in place. Nothing is appended: an adapter whose parser is
// strict (gpuworks_serve.py uses argparse.parse_args) must never see a flag
// its template did not ask for.
func TestCallableTemplateOwnsItsCommandLine(t *testing.T) {
	launch, err := callableLaunch(t, Params{
		ModelPath: "/model", APIPort: 19001, AssignedGPUs: 1, TensorParallel: 1,
		TemplateArgs: append(append([]string(nil), callableServeArgs...), "--adapter", "funasr_adapter", "--operation", "openai.audio.transcribe"),
		TemplateEnv:  map[string]string{"PYTHONPATH": "/opt/gpuworks/runtime", "WEIGHTS": "${MODEL_PATH}/weights"},
	}, ResolveLaunchInput{Entrypoint: []string{"python3", "/opt/gpuworks/runtime/gpuworks_serve.py"}})
	if err != nil {
		t.Fatal(err)
	}
	wantEntrypoint := []string{"python3", "/opt/gpuworks/runtime/gpuworks_serve.py"}
	if !reflect.DeepEqual(launch.Entrypoint, wantEntrypoint) {
		t.Fatalf("entrypoint=%v want %v", launch.Entrypoint, wantEntrypoint)
	}
	wantArgs := []string{"--model", "/model", "--host", "0.0.0.0", "--port", "19001", "--adapter", "funasr_adapter", "--operation", "openai.audio.transcribe"}
	if !reflect.DeepEqual(launch.Argv, wantArgs) {
		t.Fatalf("argv=%v want %v", launch.Argv, wantArgs)
	}
	if launch.Env["PYTHONPATH"] != "/opt/gpuworks/runtime" || launch.Env["WEIGHTS"] != "/model/weights" || launch.Env["HF_HUB_OFFLINE"] != "1" {
		t.Fatalf("env=%v", launch.Env)
	}
}

// A distributed callable launch states each rank's facts as variables; the
// template decides which of them its launcher takes and in what shape.
func TestCallableDistributedRanksReadTheirFactsFromVariables(t *testing.T) {
	args := []string{"--model", "${MODEL_PATH}", "--port", "${API_PORT}", "--tp", "${TP_SIZE}", "--gpus", "${GPU_COUNT}",
		"--rank", "${NODE_RANK}/${NNODES}", "--master", "${MASTER_ADDR}:${MASTER_PORT}", "--headless", "${HEADLESS}", "--advertise", "${NODE_ADDR}"}
	for _, test := range []struct {
		name   string
		params Params
		want   []string
	}{
		{"leader", Params{APIPort: 19001, NNodes: 2, NodeRank: 0, MasterAddr: "198.51.100.140", MasterPort: 29501, NodeAddress: "198.51.100.140"},
			[]string{"--model", "/model", "--port", "19001", "--tp", "2", "--gpus", "1", "--rank", "0/2", "--master", "198.51.100.140:29501", "--headless", "0", "--advertise", "198.51.100.140"}},
		// A headless rank was reserved no port; it says so with zero. Its own
		// address is its own, never the leader's.
		{"follower", Params{APIPort: 0, NNodes: 2, NodeRank: 1, MasterAddr: "198.51.100.140", MasterPort: 29501, Headless: true, NodeAddress: "198.51.100.141"},
			[]string{"--model", "/model", "--port", "0", "--tp", "2", "--gpus", "1", "--rank", "1/2", "--master", "198.51.100.140:29501", "--headless", "1", "--advertise", "198.51.100.141"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := test.params
			params.ModelPath, params.AssignedGPUs, params.TensorParallel, params.TemplateArgs = "/model", 1, 2, args
			launch, err := callableLaunch(t, params, ResolveLaunchInput{})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(launch.Argv, test.want) {
				t.Fatalf("argv=%v want %v", launch.Argv, test.want)
			}
		})
	}
}

// The gateway and the health check dial the port the platform reserved. A
// callable template that never tells its adapter that port would start a
// server nobody can reach, so the launch refuses it - unless the deployment's
// own command override owns the whole command.
func TestCallableTemplateMustPlaceTheAPIPort(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 19001, AssignedGPUs: 1, TensorParallel: 1, TemplateArgs: []string{"--adapter", "funasr_adapter"}}
	_, err := callableLaunch(t, params, ResolveLaunchInput{})
	if err == nil || !strings.Contains(err.Error(), "${API_PORT}") {
		t.Fatalf("ResolveLaunch() = %v, want a refusal naming ${API_PORT}", err)
	}
	launch, err := callableLaunch(t, params, ResolveLaunchInput{ContainerOverrides: ContainerOverrides{CmdDisplay: `["--port","19001"]`}})
	if err != nil {
		t.Fatalf("a command override owns the command: %v", err)
	}
	if !slices.Equal(launch.Argv, []string{"--port", "19001"}) {
		t.Fatalf("argv=%v", launch.Argv)
	}
	// A reference from the preparation script counts: it may export the port
	// to an adapter that reads its environment.
	params.TemplateArgs = nil
	if _, err := callableLaunch(t, params, ResolveLaunchInput{PreStartScript: "echo port=${API_PORT} > /tmp/port"}); err != nil {
		t.Fatalf("a script reference was not counted: %v", err)
	}
}

// A single-node launch has no master. A template that names one must fail,
// not dial an empty address.
func TestSingleNodeLaunchHasNoMasterVariables(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 19001, AssignedGPUs: 1, TensorParallel: 1,
		TemplateArgs: append(append([]string(nil), callableServeArgs...), "--master", "${MASTER_ADDR}")}
	if _, err := callableLaunch(t, params, ResolveLaunchInput{}); err == nil || !strings.Contains(err.Error(), "MASTER_ADDR is unresolved") {
		t.Fatalf("ResolveLaunch() = %v, want MASTER_ADDR unresolved", err)
	}
	// The node's own address is a distributed fact too, even when the
	// planner knows it for a single-node launch.
	params.NodeAddress = "198.51.100.140"
	params.TemplateArgs = append(append([]string(nil), callableServeArgs...), "--advertise", "${NODE_ADDR}")
	if _, err := callableLaunch(t, params, ResolveLaunchInput{}); err == nil || !strings.Contains(err.Error(), "NODE_ADDR is unresolved") {
		t.Fatalf("ResolveLaunch() = %v, want NODE_ADDR unresolved", err)
	}
	params.TemplateArgs = append(append([]string(nil), callableServeArgs...), "--nnodes", "${NNODES}", "--rank", "${NODE_RANK}", "--headless", "${HEADLESS}")
	launch, err := callableLaunch(t, params, ResolveLaunchInput{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--nnodes", "1", "--rank", "0", "--headless", "0"}; !slices.Equal(launch.Argv[len(callableServeArgs):], want) {
		t.Fatalf("argv=%v want suffix %v", launch.Argv, want)
	}
}

// The gateway and the health check send the deployment's served name as the
// request's model and rewrite it only for vLLM and SGLang. A callable adapter
// that checks the model answers under the name the template hands it, so the
// served name reaches argv, env and the entrypoint alike.
func TestCallableTemplateReceivesTheServedName(t *testing.T) {
	launch, err := callableLaunch(t, Params{
		ModelPath: "/model", ServedName: "GLM-5.3-Flash-W8A8-tdxi", APIPort: 19001, AssignedGPUs: 1, TensorParallel: 1,
		TemplateArgs: append(append([]string(nil), callableServeArgs...), "--served-model-name", "${SERVED_MODEL_NAME}"),
		TemplateEnv:  map[string]string{"ADAPTER_MODEL_NAME": "${SERVED_MODEL_NAME}"},
	}, ResolveLaunchInput{Entrypoint: []string{"bash", "-c", `exec serve --name "${SERVED_MODEL_NAME}" "$@"`, "--"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--served-model-name", "GLM-5.3-Flash-W8A8-tdxi"}; !slices.Equal(launch.Argv[len(callableServeArgs):], want) {
		t.Fatalf("argv=%v want suffix %v", launch.Argv, want)
	}
	if launch.Env["ADAPTER_MODEL_NAME"] != "GLM-5.3-Flash-W8A8-tdxi" {
		t.Fatalf("env=%v", launch.Env)
	}
	if want := `exec serve --name "GLM-5.3-Flash-W8A8-tdxi" "$@"`; launch.Entrypoint[2] != want {
		t.Fatalf("entrypoint=%v want %q", launch.Entrypoint, want)
	}
	// The platform reads the name back from the launch, as it does for vLLM,
	// so the agent's port identity probe asks for the name the engine serves.
	if got := ServedModelName(launch); got != "GLM-5.3-Flash-W8A8-tdxi" {
		t.Fatalf("ServedModelName()=%q", got)
	}
}

// A preview whose placement is not settled knows the launch's own facts but
// not its GPU count or degree: those stay symbolic, the rest is expanded.
func TestUnresolvedPreviewKeepsPlacementFactsSymbolic(t *testing.T) {
	launch, err := callableLaunch(t, Params{ModelPath: "/model", APIPort: 30000, TemplateArgs: append(append([]string(nil), callableServeArgs...), "--tp", "${TP_SIZE}", "--gpus", "${GPU_COUNT}")},
		ResolveLaunchInput{AllowUnresolved: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", "/model", "--host", "0.0.0.0", "--port", "30000", "--tp", "$TP_SIZE", "--gpus", "$GPU_COUNT"}
	if !slices.Equal(launch.Argv, want) {
		t.Fatalf("argv=%v want %v", launch.Argv, want)
	}
}

// The preparation script runs in a shell. A name the platform never states
// is the shell's; a platform name this launch lacks is still an error.
func TestPreStartScriptLeavesShellVariablesToTheShell(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 19001, AssignedGPUs: 1, TensorParallel: 1, TemplateArgs: callableServeArgs}
	launch, err := callableLaunch(t, params, ResolveLaunchInput{PreStartScript: `export PATH="${HOME}/bin:${PATH}"; ls ${MODEL_PATH}`})
	if err != nil {
		t.Fatal(err)
	}
	if want := `export PATH="${HOME}/bin:${PATH}"; ls /model`; launch.PreStartScript != want {
		t.Fatalf("script=%q want %q", launch.PreStartScript, want)
	}
	if _, err := callableLaunch(t, params, ResolveLaunchInput{PreStartScript: "ping ${MASTER_ADDR}"}); err == nil {
		t.Fatal("a single-node script referencing MASTER_ADDR was accepted")
	}
}

// On a Mac the engine is a process in the node's own network: ${API_HOST}
// must be loopback there, or the adapter serves the LAN past the gateway.
// Containers keep the callable default.
func TestCallableBindsLoopbackOnTheHostRuntime(t *testing.T) {
	for runtimeDriver, want := range map[string]string{"host": "127.0.0.1", "docker": "0.0.0.0", "udocker": "0.0.0.0", "": "0.0.0.0"} {
		launch, err := callableLaunch(t, Params{
			ModelPath: "/model", APIPort: 30001, AssignedGPUs: 1, TensorParallel: 1,
			TemplateArgs: append([]string(nil), callableServeArgs...),
			Host:         RuntimeBindHost(runtimeDriver),
		}, ResolveLaunchInput{Entrypoint: []string{"llama-server"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := argAfter(launch.Argv, "--host"); got != want {
			t.Errorf("runtime %q: --host %q, want %q (argv %v)", runtimeDriver, got, want, launch.Argv)
		}
	}
	if RuntimeBindHost("host") != "127.0.0.1" || RuntimeBindHost("docker") != "" {
		t.Fatal("RuntimeBindHost")
	}
}

func argAfter(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}
