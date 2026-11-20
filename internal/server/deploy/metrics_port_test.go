package deploy

import (
	"slices"
	"strings"
	"testing"
)

// A template that declares api.metrics.port: separate is told the port the
// platform reserved for it, in either spelling, in args, env and the
// preparation script alike; a headless rank has none and says so with zero,
// as it does for the API port.
func TestSeparateMetricsPortIsAVariableOfTheLaunch(t *testing.T) {
	args := append(append([]string(nil), callableServeArgs...), "--metrics-port", "${GPUWORKS_METRICS_PORT}", "--again", "${METRICS_PORT}")
	for _, test := range []struct {
		name   string
		params Params
		want   string
	}{
		{"serving rank", Params{APIPort: 19001, MetricsPort: 19002}, "19002"},
		{"headless rank", Params{APIPort: 0, NNodes: 2, NodeRank: 1, Headless: true, MasterAddr: "198.51.100.140", MasterPort: 29501}, "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := test.params
			params.ModelPath, params.AssignedGPUs, params.TensorParallel, params.TemplateArgs = "/model", 1, 1, args
			params.MetricsPortSeparate = true
			params.TemplateEnv = map[string]string{"METRICS_LISTEN": "127.0.0.1:${GPUWORKS_METRICS_PORT}"}
			launch, err := callableLaunch(t, params, ResolveLaunchInput{PreStartScript: "echo ${GPUWORKS_METRICS_PORT} > /tmp/metrics-port"})
			if err != nil {
				t.Fatal(err)
			}
			if tail := launch.Argv[len(callableServeArgs):]; !slices.Equal(tail, []string{"--metrics-port", test.want, "--again", test.want}) {
				t.Fatalf("argv tail=%v", tail)
			}
			if launch.Env["METRICS_LISTEN"] != "127.0.0.1:"+test.want {
				t.Fatalf("env=%v", launch.Env)
			}
			if want := "echo " + test.want + " > /tmp/metrics-port"; launch.PreStartScript != want {
				t.Fatalf("script=%q want %q", launch.PreStartScript, want)
			}
		})
	}
}

// A launch whose template never declared the port states none: a reference
// fails like any other variable nobody states, in a script as well.
func TestUndeclaredMetricsPortIsUnresolved(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 19001, MetricsPort: 19002, AssignedGPUs: 1, TensorParallel: 1,
		TemplateArgs: append(append([]string(nil), callableServeArgs...), "--metrics-port", "${GPUWORKS_METRICS_PORT}")}
	if _, err := callableLaunch(t, params, ResolveLaunchInput{}); err == nil || !strings.Contains(err.Error(), "METRICS_PORT is unresolved") {
		t.Fatalf("ResolveLaunch() = %v, want METRICS_PORT unresolved", err)
	}
	params.TemplateArgs = callableServeArgs
	if _, err := callableLaunch(t, params, ResolveLaunchInput{PreStartScript: "echo ${METRICS_PORT}"}); err == nil || !strings.Contains(err.Error(), "METRICS_PORT is unresolved") {
		t.Fatalf("script reference: ResolveLaunch() = %v, want METRICS_PORT unresolved", err)
	}
}

// A manual command shared by ranks is told the metrics port under the
// prefixed name only, and only when the template declared it.
func TestManualCommandVariablesListTheMetricsPortOnlyWhenDeclared(t *testing.T) {
	inputOf := func(separate bool) ResolveLaunchInput {
		engine, err := SelectEngine("callable")
		if err != nil {
			t.Fatal(err)
		}
		return ResolveLaunchInput{Engine: engine, Params: Params{APIPort: 19001, MetricsPort: 19002, MetricsPortSeparate: separate}}
	}
	if names := ManualCommandVariables([]ResolveLaunchInput{inputOf(true)}); !slices.Contains(names, "GPUWORKS_METRICS_PORT") {
		t.Fatalf("declared: names=%v", names)
	}
	if names := ManualCommandVariables([]ResolveLaunchInput{inputOf(false)}); slices.Contains(names, "GPUWORKS_METRICS_PORT") {
		t.Fatalf("undeclared: names=%v", names)
	}
}
