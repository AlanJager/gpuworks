package deploy

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Docker argument codes answer a deployment's own docker args that restate
// something the launch already owns. Such an argument is never silently
// dropped or overridden at run time: the request is refused and names the
// field (or platform concern) that holds the value instead.
const (
	// ErrorDockerArgPlatformManaged reports an option the platform sets on
	// every container: its name and lifecycle, its network and published
	// ports, and the accelerators the scheduler assigned.
	ErrorDockerArgPlatformManaged = "DOCKER_ARG_PLATFORM_MANAGED"
	// ErrorDockerArgUseShmField reports --shm-size, which the deployment's
	// shared memory field states.
	ErrorDockerArgUseShmField = "DOCKER_ARG_USE_SHM_FIELD"
	// ErrorDockerArgUseEnvField reports -e/--env/--env-file, which the
	// deployment's environment rows state.
	ErrorDockerArgUseEnvField = "DOCKER_ARG_USE_ENV_FIELD"
	// ErrorDockerArgUseMountField reports -v/--volume/--mount, which the
	// deployment's mount rows state.
	ErrorDockerArgUseMountField = "DOCKER_ARG_USE_MOUNT_FIELD"
	// ErrorDockerArgUseEntrypointField reports --entrypoint, which the
	// deployment's entrypoint override states.
	ErrorDockerArgUseEntrypointField = "DOCKER_ARG_USE_ENTRYPOINT_FIELD"
	// ErrorShmSizeWithHostIPC reports a shared memory size together with
	// --ipc=host: the container then uses the host's /dev/shm and the size
	// would have no effect.
	ErrorShmSizeWithHostIPC = "SHM_SIZE_WITH_HOST_IPC"
)

// managedDockerOptions maps each docker run option a deployment may not state
// to the code that says where its value lives. Aliases are listed under every
// spelling docker accepts.
var managedDockerOptions = map[string]string{
	"--name":        ErrorDockerArgPlatformManaged,
	"--detach":      ErrorDockerArgPlatformManaged,
	"--attach":      ErrorDockerArgPlatformManaged,
	"--rm":          ErrorDockerArgPlatformManaged,
	"--restart":     ErrorDockerArgPlatformManaged,
	"--network":     ErrorDockerArgPlatformManaged,
	"--net":         ErrorDockerArgPlatformManaged,
	"--publish":     ErrorDockerArgPlatformManaged,
	"--publish-all": ErrorDockerArgPlatformManaged,
	"--gpus":        ErrorDockerArgPlatformManaged,
	"--runtime":     ErrorDockerArgPlatformManaged,
	"--shm-size":    ErrorDockerArgUseShmField,
	"--env":         ErrorDockerArgUseEnvField,
	"--env-file":    ErrorDockerArgUseEnvField,
	"--volume":      ErrorDockerArgUseMountField,
	"--mount":       ErrorDockerArgUseMountField,
	"--entrypoint":  ErrorDockerArgUseEntrypointField,
}

// managedDockerShortOptions are the single-letter spellings of the managed
// options above, and shortValueOptions the docker run letters that take a
// value, after which the rest of a combined short token is that value.
var (
	managedDockerShortOptions = map[byte]string{'d': "--detach", 'a': "--attach", 'p': "--publish", 'P': "--publish-all", 'e': "--env", 'v': "--volume"}
	shortValueOptions         = "acehlmpuvw"
)

// ValidateDockerArgs judges one layer's docker args together with that same
// layer's own shared memory size: a deployment's own container_overrides, and
// a template spec's docker_args with its shm_bytes (inferencecatalog), call
// this one function so an option refused in one layer is refused in the
// other. It answers the first of DockerArgErrors. Only the layer's own args
// are judged: arguments the launch adds itself (the compute network's RDMA
// options) are the platform's, not a restatement of them.
//
// Options that hand the container the host (--privileged, --pid) are not
// refused: deployments are written by administrators, and some images need
// them. DockerArgHostAccessWarnings names them on the preview instead.
func ValidateDockerArgs(args []string, shmBytes int64) error {
	if errs := DockerArgErrors(args, shmBytes); len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// DockerArgErrors answers every docker arg ValidateDockerArgs refuses, in
// argument order, so a form shows all of them at once instead of one per
// correction.
func DockerArgErrors(args []string, shmBytes int64) []error {
	var errs []error
	for index, arg := range args {
		token := strings.TrimSpace(arg)
		option := dockerOptionName(token)
		if option == "" {
			continue
		}
		if code, managed := managedDockerOptions[option]; managed {
			errs = append(errs, messages.NewProductError(code, "docker arg "+token+" restates "+option+", which the deployment does not take from docker args", map[string]any{"arg": token, "option": option}))
			continue
		}
		if option == "--ipc" && shmBytes > 0 && dockerOptionValue(args, index) == "host" {
			errs = append(errs, messages.NewProductError(ErrorShmSizeWithHostIPC, "a shared memory size has no effect with --ipc=host", map[string]any{"arg": token}))
		}
	}
	return errs
}

// DockerArgHostAccessWarnings names each docker arg that hands the container
// the host: --privileged (every device, the host's kernel interfaces) and
// --pid (the host's or another container's processes, which the container can
// then read and signal). They are allowed, so the preview says what they do.
func DockerArgHostAccessWarnings(args []string) []messages.Warning {
	var warnings []messages.Warning
	for index, arg := range args {
		token := strings.TrimSpace(arg)
		var code string
		switch dockerOptionName(token) {
		case "--privileged":
			if value, stated := inlineDockerOptionValue(token); stated && value == "false" {
				continue
			}
			code = messages.WarningDockerArgPrivileged
		case "--pid":
			if dockerOptionValue(args, index) == "" {
				continue
			}
			code = messages.WarningDockerArgSharedPID
		default:
			continue
		}
		warnings = append(warnings, messages.NewWarning(code, map[string]any{"arg": token, "option": dockerOptionName(token)}))
	}
	return warnings
}

// DockerArgsShareHostIPC answers whether docker args put the container in the
// host's IPC namespace, where /dev/shm is the host's own and no size applies.
func DockerArgsShareHostIPC(args []string) bool {
	for index, arg := range args {
		if dockerOptionName(strings.TrimSpace(arg)) == "--ipc" && dockerOptionValue(args, index) == "host" {
			return true
		}
	}
	return false
}

// shmSizeWithTemplateHostIPC reports the cross-layer form of
// ErrorShmSizeWithHostIPC: the deployment's own shm_bytes has no effect
// because the selected template's docker_args already put the container on
// the host's IPC namespace. The deployment did not state --ipc=host itself,
// so ValidateDockerArgs (which only sees the deployment's own args)
// cannot catch this; it is checked once the template layer is known, beside
// launchShmBytes.
func shmSizeWithTemplateHostIPC() error {
	return messages.NewProductError(ErrorShmSizeWithHostIPC,
		"a shared memory size has no effect with --ipc=host, which the template sets",
		map[string]any{"arg": "--ipc=host"})
}

// dockerOptionName answers the long option a docker args token names, or ""
// for a value token. A token may carry its value after "=" or, as the deploy
// form's one-row-per-argument editor allows, after a space.
func dockerOptionName(token string) string {
	if strings.HasPrefix(token, "--") {
		name, _, _ := strings.Cut(token, "=")
		name, _, _ = strings.Cut(name, " ")
		return name
	}
	if !strings.HasPrefix(token, "-") || len(token) < 2 {
		return ""
	}
	// A combined short token such as -it or -dp8000:8000: every letter up to
	// the first value-taking one is its own flag.
	for i := 1; i < len(token); i++ {
		letter := token[i]
		if option, managed := managedDockerShortOptions[letter]; managed {
			return option
		}
		if strings.IndexByte(shortValueOptions, letter) >= 0 {
			break
		}
	}
	return ""
}

// dockerOptionValue answers the value of the option at args[index], in any of
// the forms dockerOptionName accepts.
func dockerOptionValue(args []string, index int) string {
	token := strings.TrimSpace(args[index])
	if _, value, ok := strings.Cut(token, "="); ok {
		return strings.TrimSpace(value)
	}
	if _, value, ok := strings.Cut(token, " "); ok {
		return strings.TrimSpace(value)
	}
	if index+1 < len(args) {
		return strings.TrimSpace(args[index+1])
	}
	return ""
}

// inlineDockerOptionValue answers the value a token carries itself, after "="
// or a space, for a flag option such as --privileged=false.
func inlineDockerOptionValue(token string) (string, bool) {
	if _, value, ok := strings.Cut(token, "="); ok {
		return strings.TrimSpace(value), true
	}
	if _, value, ok := strings.Cut(token, " "); ok {
		return strings.TrimSpace(value), true
	}
	return "", false
}
