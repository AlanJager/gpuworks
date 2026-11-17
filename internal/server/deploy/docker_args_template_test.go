package deploy

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Launch docker args apply in layers: the selected template spec's own
// docker_args first, then the deployment's own, then the platform's
// (compute network, memlock) - the same order shm_bytes and memlock_unlimited
// take.
func TestResolveLaunchOrdersTemplateThenOwnThenPlatformDockerArgs(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1}
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{}, Params: params,
		TemplateDockerArgs: []string{"--cap-add=SYS_PTRACE"},
		ContainerOverrides: ContainerOverrides{DockerArgs: []string{"--cap-add=SYS_NICE"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--cap-add=SYS_PTRACE", "--cap-add=SYS_NICE"}
	if !reflect.DeepEqual(launch.DockerArgs, want) {
		t.Fatalf("docker args = %v, want %v", launch.DockerArgs, want)
	}
}

// A spec without docker_args leaves the launch exactly as it was before the
// field existed: byte-identical docker args to a launch with no template
// docker args stated at all.
func TestResolveLaunchWithoutTemplateDockerArgsIsUnchanged(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1}
	withField, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{}, Params: params, TemplateDockerArgs: nil,
		ContainerOverrides: ContainerOverrides{DockerArgs: []string{"--cap-add", "SYS_NICE"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	withoutField, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{}, Params: params,
		ContainerOverrides: ContainerOverrides{DockerArgs: []string{"--cap-add", "SYS_NICE"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withField.DockerArgs, withoutField.DockerArgs) {
		t.Fatalf("docker args with nil TemplateDockerArgs = %v, want %v", withField.DockerArgs, withoutField.DockerArgs)
	}
}

// The memlock dedupe (hasMemlockUlimit) sees the combined template+own args:
// a template that states its own ulimit is not repeated by memlock_unlimited.
func TestResolveLaunchMemlockDedupeSeesTemplateDockerArgs(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1}
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{}, Params: params, TemplateMemlockUnlimited: true,
		TemplateDockerArgs: []string{"--ulimit", "memlock=4096"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := memlockArgs(launch.DockerArgs); !reflect.DeepEqual(got, []string{"memlock=4096"}) {
		t.Fatalf("memlock ulimits = %v in %v, want the template's own", got, launch.DockerArgs)
	}
}

// A deployment's own shm_bytes together with a template's --ipc=host is
// refused: the deployment's own args did not state --ipc=host, so only the
// combined check (which knows the template) catches it, and it is not
// silently dropped to the host's /dev/shm.
func TestResolveLaunchRefusesOwnShmWithTemplateHostIPC(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1}
	for _, templateArgs := range [][]string{{"--ipc=host"}, {"--ipc", "host"}, {"--ipc host"}} {
		_, err := ResolveLaunch(ResolveLaunchInput{
			Engine: launchTestEngine{}, Params: params, TemplateDockerArgs: templateArgs,
			ContainerOverrides: ContainerOverrides{ShmBytes: 32 << 30},
		})
		var productErr *messages.ProductError
		if !errors.As(err, &productErr) || productErr.Code() != ErrorShmSizeWithHostIPC {
			t.Fatalf("%v: err=%v, want %s", templateArgs, err, ErrorShmSizeWithHostIPC)
		}
	}
	// Without a stated shm_bytes the template's host IPC is simply in effect:
	// no conflict, and the launch states no size.
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{}, Params: params, TemplateDockerArgs: []string{"--ipc=host"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if launch.ShmBytes != 0 {
		t.Fatalf("shm = %d, want none under the template's host IPC", launch.ShmBytes)
	}
	// The deployment's own --ipc=host (not the template's) is still judged by
	// ValidateContainerDockerArgs at decode time, unchanged.
	_, err = DecodeContainerOverrides([]byte(`{"docker_args":["--ipc=host"],"shm_bytes":34359738368}`))
	var productErr *messages.ProductError
	if !errors.As(err, &productErr) || productErr.Code() != ErrorShmSizeWithHostIPC {
		t.Fatalf("own --ipc=host + own shm: err=%v, want %s", err, ErrorShmSizeWithHostIPC)
	}
}

// ValidateDockerArgs is the one function a deployment's own
// container_overrides.docker_args and a template spec's docker_args both call:
// it refuses a host-escaping option before judging a platform-managed
// restatement and --ipc=host together with the same layer's own shm_bytes.
func TestValidateDockerArgsIsOneFunctionForBothLayers(t *testing.T) {
	if err := ValidateDockerArgs([]string{"--privileged"}, 0); err == nil {
		t.Fatal("--privileged accepted")
	}
	if err := ValidateDockerArgs([]string{"--network=host"}, 0); err == nil {
		t.Fatal("--network=host accepted")
	}
	if err := ValidateDockerArgs([]string{"--ipc=host"}, 8<<30); err == nil {
		t.Fatal("--ipc=host with a shm size accepted")
	}
	if err := ValidateDockerArgs([]string{"--cap-add=SYS_PTRACE", "--security-opt=seccomp=unconfined"}, 0); err != nil {
		t.Fatalf("ordinary docker args rejected: %v", err)
	}
}

// Whitespace in a one-row-per-argument docker arg is not mistaken for the
// managed options, and launchDockerArgs never mutates the slices it combines.
func TestLaunchDockerArgsDoesNotMutateTemplateOrOwnArgs(t *testing.T) {
	template := make([]string, 1, 4)
	template[0] = "--cap-add=SYS_PTRACE"
	own := make([]string, 1, 4)
	own[0] = "--cap-add=SYS_NICE"
	got := launchDockerArgs(template, own, Params{}, false)
	want := []string{"--cap-add=SYS_PTRACE", "--cap-add=SYS_NICE"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(template, []string{"--cap-add=SYS_PTRACE"}) || len(template[:cap(template)]) != 4 || template[:2][1] != "" {
		t.Fatalf("template args changed: %v", template[:2])
	}
	if !reflect.DeepEqual(own, []string{"--cap-add=SYS_NICE"}) || len(own[:cap(own)]) != 4 || own[:2][1] != "" {
		t.Fatalf("own args changed: %v", own[:2])
	}
}
