package inferencecatalog

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// validateDockerArgs checks a spec's docker_args: an empty or whitespace-only
// entry is a catalog authoring mistake (the same shape check pre_start_argv
// gets), and the options themselves are judged exactly like a deployment's
// own container_overrides.docker_args (deploy.ValidateDockerArgs), against
// this same spec's shm_bytes — a template's docker_args stating --ipc=host
// together with its own shm_bytes is refused here, before a deployment ever
// selects the spec.
func validateDockerArgs(modelID string, spec *InferenceSpec) error {
	for i, arg := range spec.DockerArgs {
		if strings.TrimSpace(arg) == "" {
			return fmt.Errorf("model %s spec %s docker_args[%d] is empty", modelID, spec.Name, i)
		}
	}
	if err := deploy.ValidateDockerArgs(spec.DockerArgs, spec.ShmBytes); err != nil {
		return fmt.Errorf("model %s spec %s docker_args: %w", modelID, spec.Name, err)
	}
	return nil
}
