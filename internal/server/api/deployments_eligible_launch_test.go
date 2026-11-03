package api

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// Keep the real adapter's assembly and hooks, with one architecture-dependent
// launch option. This exposes callers that resolve without actual card facts.
type architectureFeatureEngine struct {
	deploy.Engine
	deploy.EngineHooks
}

func (e architectureFeatureEngine) Build(p deploy.Params) deploy.ResolvedLaunch {
	launch := e.Engine.Build(p)
	if p.ArchID == "gfx1151" {
		launch.Argv = append(launch.Argv, "--pipeline-parallel-size", "2")
	}
	return launch
}
