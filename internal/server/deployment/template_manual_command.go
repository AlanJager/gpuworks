package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// applySpecManualCommand starts a deployment of a spec that owns its launch
// (InferenceSpecRow.ManualCommand) on that script as its manual command,
// unless the request states a command or a command mode of its own: the
// script is the template's default, and the deployer may still replace it or
// go back to the engine-built command by naming a mode. The deployment keeps
// the script it was created with; a later catalog change does not reach it.
// It reports whether the default was applied.
func applySpecManualCommand(req *dto.DeployRequest, spec *store.InferenceSpecRow) bool {
	if spec == nil || strings.TrimSpace(spec.ManualCommand) == "" {
		return false
	}
	cmd, _ := req.ContainerOverrides["cmd_display"].(string)
	mode, _ := req.ContainerOverrides["command_mode"].(string)
	if strings.TrimSpace(cmd) != "" || strings.TrimSpace(mode) != "" {
		return false
	}
	if req.ContainerOverrides == nil {
		req.ContainerOverrides = map[string]any{}
	}
	req.ContainerOverrides["command_mode"] = string(deploy.CommandManual)
	req.ContainerOverrides["cmd_display"] = spec.ManualCommand
	return true
}
