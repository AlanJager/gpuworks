package deploy

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
)

func TestValidateContainerDockerArgsRejectsEachManagedSpelling(t *testing.T) {
	cases := []struct {
		args   []string
		code   string
		option string
	}{
		{[]string{"--name", "mine"}, ErrorDockerArgPlatformManaged, "--name"},
		{[]string{"-d"}, ErrorDockerArgPlatformManaged, "--detach"},
		{[]string{"-dit"}, ErrorDockerArgPlatformManaged, "--detach"},
		{[]string{"--rm"}, ErrorDockerArgPlatformManaged, "--rm"},
		{[]string{"--restart=always"}, ErrorDockerArgPlatformManaged, "--restart"},
		{[]string{"--network", "bridge"}, ErrorDockerArgPlatformManaged, "--network"},
		{[]string{"--net=host"}, ErrorDockerArgPlatformManaged, "--net"},
		{[]string{"-p", "8000:8000"}, ErrorDockerArgPlatformManaged, "--publish"},
		{[]string{"-p8000:8000"}, ErrorDockerArgPlatformManaged, "--publish"},
		{[]string{"-P"}, ErrorDockerArgPlatformManaged, "--publish-all"},
		{[]string{"--gpus", "all"}, ErrorDockerArgPlatformManaged, "--gpus"},
		{[]string{"--runtime=nvidia"}, ErrorDockerArgPlatformManaged, "--runtime"},
		// The deploy form's editor keeps one argument per row, so a row can
		// carry its value after a space.
		{[]string{"--shm-size 8g"}, ErrorDockerArgUseShmField, "--shm-size"},
		{[]string{"--shm-size=8g"}, ErrorDockerArgUseShmField, "--shm-size"},
		{[]string{"-e", "A=1"}, ErrorDockerArgUseEnvField, "--env"},
		{[]string{"--env-file", "/etc/env"}, ErrorDockerArgUseEnvField, "--env-file"},
		{[]string{"-v", "/data:/data"}, ErrorDockerArgUseMountField, "--volume"},
		{[]string{"--mount", "type=bind,src=/a,dst=/b"}, ErrorDockerArgUseMountField, "--mount"},
		{[]string{"--entrypoint=/bin/sh"}, ErrorDockerArgUseEntrypointField, "--entrypoint"},
	}
	for _, tc := range cases {
		err := ValidateContainerDockerArgs(tc.args, 0)
		var productErr *messages.ProductError
		if !errors.As(err, &productErr) {
			t.Fatalf("%v: err=%v, want a product error", tc.args, err)
		}
		if productErr.Code() != tc.code || productErr.Params()["option"] != tc.option {
			t.Fatalf("%v: code=%s params=%v, want %s for %s", tc.args, productErr.Code(), productErr.Params(), tc.code, tc.option)
		}
	}
}

func TestValidateContainerDockerArgsKeepsOrdinaryOptions(t *testing.T) {
	args := []string{
		"--ipc=host", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK",
		"--device", "/dev/infiniband", "-it", "-m", "64g", "-u", "1000", "--security-opt", "seccomp=unconfined",
	}
	if err := ValidateContainerDockerArgs(args, 0); err != nil {
		t.Fatalf("ordinary docker args rejected: %v", err)
	}
	// A value token is not an option even when it begins with a dash.
	if err := ValidateContainerDockerArgs([]string{"--ulimit", "-1"}, 0); err != nil {
		t.Fatalf("value token judged as an option: %v", err)
	}
}

func TestValidateContainerDockerArgsRejectsShmWithHostIPC(t *testing.T) {
	for _, args := range [][]string{{"--ipc=host"}, {"--ipc", "host"}, {"--ipc host"}} {
		err := ValidateContainerDockerArgs(args, 32<<30)
		var productErr *messages.ProductError
		if !errors.As(err, &productErr) || productErr.Code() != ErrorShmSizeWithHostIPC {
			t.Fatalf("%v: err=%v, want %s", args, err, ErrorShmSizeWithHostIPC)
		}
	}
	// Without a stated size the engine default is simply unused under host
	// IPC, which is the documented vLLM setup and not a conflict.
	if err := ValidateContainerDockerArgs([]string{"--ipc=host"}, 0); err != nil {
		t.Fatalf("host IPC without a size rejected: %v", err)
	}
	if err := ValidateContainerDockerArgs([]string{"--ipc=shareable"}, 32<<30); err != nil {
		t.Fatalf("private IPC with a size rejected: %v", err)
	}
}

func TestResolveLaunchShmFollowsProfileThenOverride(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1}
	defaulted, err := ResolveLaunch(ResolveLaunchInput{Engine: vLLMEngine{}, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.ShmBytes != (vLLMEngine{}).Profile().ShmBytes || defaulted.ShmBytes != 16<<30 {
		t.Fatalf("default shm = %d, want the engine profile's 16 GiB", defaulted.ShmBytes)
	}
	overridden, err := ResolveLaunch(ResolveLaunchInput{
		Engine: vLLMEngine{}, Params: params,
		ContainerOverrides: ContainerOverrides{ShmBytes: 64 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	if overridden.ShmBytes != 64<<30 {
		t.Fatalf("overridden shm = %d, want 64 GiB", overridden.ShmBytes)
	}
	// Under host IPC the container's /dev/shm is the host's: the launch states
	// no size, so neither the preview nor the agent's --shm-size claims one.
	for _, args := range [][]string{{"--ipc=host"}, {"--ipc", "host"}, {"--ipc host"}} {
		hostIPC, err := ResolveLaunch(ResolveLaunchInput{
			Engine: vLLMEngine{}, Params: params,
			ContainerOverrides: ContainerOverrides{DockerArgs: args},
		})
		if err != nil {
			t.Fatal(err)
		}
		if hostIPC.ShmBytes != 0 {
			t.Fatalf("%v: shm = %d, want none under host IPC", args, hostIPC.ShmBytes)
		}
	}
}

func TestResolveLaunchRefusesManagedDockerArgsInsteadOfDroppingThem(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1}
	_, err := ResolveLaunch(ResolveLaunchInput{
		Engine: vLLMEngine{}, Params: params,
		ContainerOverrides: ContainerOverrides{DockerArgs: []string{"--shm-size=64g"}},
	})
	var productErr *messages.ProductError
	if !errors.As(err, &productErr) || productErr.Code() != ErrorDockerArgUseShmField {
		t.Fatalf("err=%v, want %s", err, ErrorDockerArgUseShmField)
	}
	// Arguments the launch adds itself are not judged as a restatement: the
	// compute network's RDMA options reach docker args beside the
	// deployment's own.
	rdma := Params{NNodes: 2, ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostRDMA}}
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{}, Params: rdma,
		ContainerOverrides: ContainerOverrides{DockerArgs: []string{"--ulimit", "stack=67108864"}},
	})
	want := []string{"--ulimit", "stack=67108864", "--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"}
	if err != nil || !reflect.DeepEqual(launch.DockerArgs, want) {
		t.Fatalf("docker args = %v err=%v, want %v", launch.DockerArgs, err, want)
	}
}

func TestContainerOverridesDecodeAndValidateShm(t *testing.T) {
	overrides, err := DecodeContainerOverrides([]byte(`{"shm_bytes":8589934592}`))
	if err != nil || overrides.ShmBytes != 8<<30 {
		t.Fatalf("overrides=%+v err=%v", overrides, err)
	}
	if _, err := DecodeContainerOverrides([]byte(`{"shm_bytes":-1}`)); err == nil {
		t.Fatal("negative shm accepted")
	}
	for _, empty := range []string{"", "null", "{}"} {
		if overrides, err := DecodeContainerOverrides([]byte(empty)); err != nil || !reflect.DeepEqual(overrides, ContainerOverrides{}) {
			t.Fatalf("%q decoded to %+v err=%v", empty, overrides, err)
		}
	}
}
