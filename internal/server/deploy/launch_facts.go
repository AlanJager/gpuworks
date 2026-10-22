package deploy

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/proto"
)

// RedactLaunchText applies the same conservative secret filtering used for
// external command logs to a free-form launch fact.
func RedactLaunchText(value string) string {
	return commandlog.RedactScript(value)
}

// RedactLaunchEnv returns a detached, redacted environment projection.
func RedactLaunchEnv(values map[string]string) map[string]string {
	return commandlog.RedactEnv(values)
}

// RedactResolvedLaunch removes secrets and embedded runtime asset content from
// the resolved launch projection exposed by the read-only launch-facts API.
func RedactResolvedLaunch(in proto.ResolvedLaunch) proto.ResolvedLaunch {
	out := in
	out.Entrypoint = commandlog.RedactArgv(in.Entrypoint)
	out.Argv = commandlog.RedactArgv(in.Argv)
	out.PreStartArgv = commandlog.RedactArgv(in.PreStartArgv)
	out.PreStartScript = commandlog.RedactScript(in.PreStartScript)
	out.Env = commandlog.RedactEnv(in.Env)
	out.EnvLayers.System = commandlog.RedactEnv(in.EnvLayers.System)
	out.EnvLayers.Template = commandlog.RedactEnv(in.EnvLayers.Template)
	out.EnvLayers.User = commandlog.RedactEnv(in.EnvLayers.User)
	out.DockerArgs = commandlog.RedactArgv(in.DockerArgs)
	out.ModelMount = commandlog.RedactScript(in.ModelMount)
	out.HealthPath = commandlog.RedactScript(in.HealthPath)
	out.RuntimeAssets = append([]proto.RuntimeAsset(nil), in.RuntimeAssets...)
	for index := range out.RuntimeAssets {
		out.RuntimeAssets[index].Content = nil
		out.RuntimeAssets[index].File = commandlog.RedactScript(out.RuntimeAssets[index].File)
		out.RuntimeAssets[index].UnpackTo = commandlog.RedactScript(out.RuntimeAssets[index].UnpackTo)
		out.RuntimeAssets[index].ModelID = commandlog.RedactScript(out.RuntimeAssets[index].ModelID)
	}
	out.Mounts = append([]proto.MountSpec(nil), in.Mounts...)
	for index := range out.Mounts {
		out.Mounts[index].Source = commandlog.RedactScript(out.Mounts[index].Source)
		out.Mounts[index].Volume = commandlog.RedactScript(out.Mounts[index].Volume)
		out.Mounts[index].Target = commandlog.RedactScript(out.Mounts[index].Target)
		out.Mounts[index].SizeHint = commandlog.RedactScript(out.Mounts[index].SizeHint)
		out.Mounts[index].Purpose = commandlog.RedactScript(out.Mounts[index].Purpose)
	}
	if in.PlacementVariables != nil {
		out.PlacementVariables, _ = commandlog.RedactStructured(in.PlacementVariables).(map[string]any)
	}
	if len(in.APIContract) > 0 {
		var contract any
		if json.Unmarshal(in.APIContract, &contract) == nil {
			out.APIContract, _ = json.Marshal(commandlog.RedactStructured(contract))
		} else {
			out.APIContract = nil
		}
	}
	return out
}

// RedactRuntimeObservation returns a detached, redacted observation. Layer
// status validation and unknown-value normalization remain the API projection's
// responsibility.
func RedactRuntimeObservation(in proto.InstanceRuntimeObservation) proto.InstanceRuntimeObservation {
	out := in
	out.Runtime = commandlog.RedactScript(in.Runtime)
	out.Container.Entrypoint = commandlog.RedactArgv(in.Container.Entrypoint)
	out.Container.Cmd = commandlog.RedactArgv(in.Container.Cmd)
	out.Container.Args = commandlog.RedactArgv(in.Container.Args)
	out.Container.Env = commandlog.RedactEnv(in.Container.Env)
	out.Container.Mounts = append([]proto.RuntimeMountObservation(nil), in.Container.Mounts...)
	for index := range out.Container.Mounts {
		out.Container.Mounts[index].Type = commandlog.RedactScript(out.Container.Mounts[index].Type)
		out.Container.Mounts[index].Source = commandlog.RedactScript(out.Container.Mounts[index].Source)
		out.Container.Mounts[index].Destination = commandlog.RedactScript(out.Container.Mounts[index].Destination)
	}
	out.Container.Devices = commandlog.RedactArgv(in.Container.Devices)
	out.Container.Path = commandlog.RedactScript(in.Container.Path)
	out.Container.RuntimeName = commandlog.RedactScript(in.Container.RuntimeName)
	out.Container.NetworkMode = commandlog.RedactScript(in.Container.NetworkMode)
	out.Image.Entrypoint = commandlog.RedactArgv(in.Image.Entrypoint)
	out.Image.Cmd = commandlog.RedactArgv(in.Image.Cmd)
	out.Image.Env = commandlog.RedactEnv(in.Image.Env)
	out.Image.Volumes = commandlog.RedactArgv(in.Image.Volumes)
	out.Process.Argv = commandlog.RedactArgv(in.Process.Argv)
	out.Process.Env = commandlog.RedactEnv(in.Process.Env)
	out.Process.Identity = commandlog.RedactScript(in.Process.Identity)
	return out
}
