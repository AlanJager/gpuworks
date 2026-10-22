package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ContainerOverrides is the container fields one deployment sets or clears
// itself (CONTEXT.md, 容器覆盖). It is an input to ResolveLaunch, not a
// complete run definition: every field left empty keeps what the engine, the
// selected template and placement produced. The image catalog contributes
// nothing here (ADR 0031); an image is only the artifact the container runs.
//
// The wire keys are what the deploy form and API clients send in
// container_overrides.
type ContainerOverrides struct {
	// ModelMountPath is where the weights are mounted and what the command's
	// model path is rewritten to. Empty keeps DefaultModelMount, or the
	// template component's primary mount, which this may not contradict.
	ModelMountPath string            `json:"model_mount_path,omitempty"`
	ExtraMounts    []ContainerMount  `json:"extra_mounts,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	// EnvUnset removes keys every lower layer set (engine, compute network,
	// template) before Env applies. Accelerator and dGPU variables the agent
	// injects on the node are not launch env and are not affected.
	EnvUnset   []string `json:"env_unset,omitempty"`
	DockerArgs []string `json:"docker_args,omitempty"`
	// EntrypointDisplay replaces the executable (JSON array or shell words).
	EntrypointDisplay string `json:"entrypoint_display,omitempty"`
	// CmdDisplay is the complete command: a JSON argv, or in manual mode a
	// shell script.
	CmdDisplay  string `json:"cmd_display,omitempty"`
	CommandMode string `json:"command_mode,omitempty"`
	// ShmBytes is the container's /dev/shm size. Zero takes the derived size
	// (the engine profile's default plus the CPU KV tier, see launchShmBytes);
	// a size below the CPU KV tier is refused. Docker args may not state it.
	ShmBytes int64 `json:"shm_bytes,omitempty"`
	// CommandIncludesPreparation says a complete manual script owns the
	// template's preparation as well as the engine command. It cannot be
	// inferred from arbitrary shell text, so it is stated.
	CommandIncludesPreparation bool `json:"command_includes_preparation,omitempty"`
}

// ContainerMount is one extra mount row: a host directory or a named volume
// at a container path.
type ContainerMount struct {
	HostPath      string `json:"host_path,omitempty"`
	Volume        string `json:"volume,omitempty"`
	ContainerPath string `json:"container_path"`
	SizeHint      string `json:"size_hint,omitempty"`
	ReadOnly      bool   `json:"read_only,omitempty"`
	Purpose       string `json:"purpose,omitempty"`
}

// DecodeContainerOverrides reads the container_overrides object of a request
// or a stored deployment row and validates it. An absent object is the empty
// override.
func DecodeContainerOverrides(raw []byte) (ContainerOverrides, error) {
	var overrides ContainerOverrides
	if trimmed := strings.TrimSpace(string(raw)); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(raw, &overrides); err != nil {
			return ContainerOverrides{}, fmt.Errorf("invalid container overrides: %w", err)
		}
	}
	return overrides, overrides.Validate()
}

// Validate applies the rules a deployment's own container fields are held to.
// Rules that need the rest of the launch (mount collisions with platform
// mounts, the component primary mount) are ResolveLaunch's.
func (o ContainerOverrides) Validate() error {
	switch CommandMode(o.CommandMode) {
	case "", CommandEngine, CommandImageEntry, CommandManual:
	default:
		return fmt.Errorf("container overrides command_mode %q is invalid", o.CommandMode)
	}
	if o.CommandIncludesPreparation && (strings.TrimSpace(o.CmdDisplay) == "" || (o.CommandMode != "" && CommandMode(o.CommandMode) != CommandManual)) {
		return errors.New("command_includes_preparation requires a manual command override")
	}
	if o.ShmBytes < 0 {
		return fmt.Errorf("container overrides shm_bytes %d is negative", o.ShmBytes)
	}
	if modelPath := strings.TrimSpace(o.ModelMountPath); modelPath != "" && (!strings.HasPrefix(modelPath, "/") || strings.Contains(modelPath, "..")) {
		return fmt.Errorf("container overrides model_mount_path %q is not an absolute safe path", o.ModelMountPath)
	}
	for key := range o.Env {
		if strings.TrimSpace(key) == "" {
			return errors.New("container overrides env key is empty")
		}
	}
	for _, key := range o.EnvUnset {
		if strings.TrimSpace(key) == "" {
			return errors.New("container overrides env_unset key is empty")
		}
	}
	for _, arg := range o.DockerArgs {
		if err := validateUnsafeDockerArg(arg); err != nil {
			return err
		}
	}
	if err := ValidateContainerDockerArgs(o.DockerArgs, o.ShmBytes); err != nil {
		return err
	}
	for _, mount := range o.ExtraMounts {
		containerPath := strings.TrimSpace(mount.ContainerPath)
		if containerPath == "" || !strings.HasPrefix(containerPath, "/") || strings.Contains(containerPath, "..") {
			return errors.New("container overrides mount container_path must be absolute")
		}
		hostPath := strings.TrimSpace(mount.HostPath)
		if hostPath == "" && strings.TrimSpace(mount.Volume) == "" {
			return fmt.Errorf("container overrides mount %q needs host_path or volume", mount.ContainerPath)
		}
		if hostPath == "/" || strings.Contains(mount.HostPath, "..") {
			return fmt.Errorf("container overrides mount host_path %q is not allowed", mount.HostPath)
		}
	}
	return nil
}

// validateUnsafeDockerArg refuses options that would hand the container the
// host: they are not a restatement of a platform field, they are never allowed.
func validateUnsafeDockerArg(arg string) error {
	trimmed := strings.TrimSpace(arg)
	for _, blocked := range []string{"--privileged", "--pid=host", "--pid", "-v /", "--volume /"} {
		if trimmed == blocked || strings.HasPrefix(trimmed, blocked+"=") || strings.HasPrefix(trimmed, blocked+":") {
			return fmt.Errorf("docker arg %q is not allowed", arg)
		}
	}
	return nil
}

func convertMounts(values []ContainerMount) []proto.MountSpec {
	result := make([]proto.MountSpec, 0, len(values))
	for _, mount := range values {
		result = append(result, proto.MountSpec{
			Source: mount.HostPath, Volume: mount.Volume, Target: mount.ContainerPath,
			ReadOnly: mount.ReadOnly, SizeHint: mount.SizeHint, Purpose: mount.Purpose,
		})
	}
	return result
}
