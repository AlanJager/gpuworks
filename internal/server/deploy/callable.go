package deploy

import (
	"gpuworks.ai/gpuworks/internal/proto"
	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

// callableEngine hosts a catalog-owned Serve Adapter. The adapter, its
// model-specific configuration and its whole command line are directory
// content; this engine appends no argument of its own.
type callableEngine struct{ BaseEngine }

func (callableEngine) Name() string { return EngineCallable }

// APIBindHost keeps the adapter reachable from outside its own loopback, which
// is the address callable adapters have always been told to bind.
func (callableEngine) APIBindHost() string { return "0.0.0.0" }

// RequiredTemplateVariables: the gateway and the health check dial the port
// the platform reserved, and only the template can tell the adapter about it.
func (callableEngine) RequiredTemplateVariables() []string {
	return []string{placementeval.VarAPIPort}
}

func (callableEngine) Profile() Profile {
	return Profile{
		Name: EngineCallable, HealthPath: "/health", ShmBytes: 16 << 30,
		MetricPath: "/metrics", MetricPrefix: "gpuworks_callable",
		// The adapter is a catalog-owned process, so the platform has no
		// measurement of its utilization of its own. It is sized with the
		// measured default engine assumption rather than with nothing: an
		// unstated utilization has to reach the estimator as a number, and the
		// scheduler rejects a plan made with zero.
		GPUMemUtilDefault: 0.9,
	}
}

func (callableEngine) Build(params Params) ResolvedLaunch {
	// The adapter's command line belongs to the template. Platform facts -
	// model path, bind address, API port, distributed rank - reach it as
	// template variables (launchVariables) that the template places itself.
	args := append(append([]string(nil), params.TemplateArgs...), params.ExtraArgs...)
	env := map[string]string{"HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1"}
	for key, value := range params.TemplateEnv {
		env[key] = value
	}
	return ResolvedLaunch{
		Entrypoint: []string{"gpuworks-callable"}, Argv: args, Env: env,
		EnvLayers: proto.LaunchEnvLayers{Template: cloneEnv(params.TemplateEnv)},
	}
}
