package deploy

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
)

func TestValidateDockerArgsRejectsEachManagedSpelling(t *testing.T) {
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
		err := ValidateDockerArgs(tc.args, 0)
		var productErr *messages.ProductError
		if !errors.As(err, &productErr) {
			t.Fatalf("%v: err=%v, want a product error", tc.args, err)
		}
		if productErr.Code() != tc.code || productErr.Params()["option"] != tc.option {
			t.Fatalf("%v: code=%s params=%v, want %s for %s", tc.args, productErr.Code(), productErr.Params(), tc.code, tc.option)
		}
	}
}

func TestValidateDockerArgsKeepsOrdinaryOptions(t *testing.T) {
	args := []string{
		"--ipc=host", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK",
		"--device", "/dev/infiniband", "-it", "-m", "64g", "-u", "1000", "--security-opt", "seccomp=unconfined",
	}
	if err := ValidateDockerArgs(args, 0); err != nil {
		t.Fatalf("ordinary docker args rejected: %v", err)
	}
	// A value token is not an option even when it begins with a dash.
	if err := ValidateDockerArgs([]string{"--ulimit", "-1"}, 0); err != nil {
		t.Fatalf("value token judged as an option: %v", err)
	}
}

func TestValidateDockerArgsRejectsShmWithHostIPC(t *testing.T) {
	for _, args := range [][]string{{"--ipc=host"}, {"--ipc", "host"}, {"--ipc host"}} {
		err := ValidateDockerArgs(args, 32<<30)
		var productErr *messages.ProductError
		if !errors.As(err, &productErr) || productErr.Code() != ErrorShmSizeWithHostIPC {
			t.Fatalf("%v: err=%v, want %s", args, err, ErrorShmSizeWithHostIPC)
		}
	}
	// Without a stated size the engine default is simply unused under host
	// IPC, which is the documented vLLM setup and not a conflict.
	if err := ValidateDockerArgs([]string{"--ipc=host"}, 0); err != nil {
		t.Fatalf("host IPC without a size rejected: %v", err)
	}
	if err := ValidateDockerArgs([]string{"--ipc=shareable"}, 32<<30); err != nil {
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

// Every refused arg is answered at once, in argument order, and an allowed
// host-access option ahead of them does not hide them: the deploy form shows
// them all beside the docker args field.
func TestDockerArgErrorsAnswersEveryRefusedArg(t *testing.T) {
	args := []string{"--shm-size 256g", "--network=host ", "--name Qwen3.8_INT", "--privileged",
		"--device=/dev/kfd", "--group-add video ", "--cap-add=SYS_PTRACE", "--security-opt seccomp=unconfined"}
	errs := DockerArgErrors(args, 0)
	want := []struct{ code, arg string }{
		{ErrorDockerArgUseShmField, "--shm-size 256g"},
		{ErrorDockerArgPlatformManaged, "--network=host"},
		{ErrorDockerArgPlatformManaged, "--name Qwen3.8_INT"},
	}
	if len(errs) != len(want) {
		t.Fatalf("errors = %v, want %d", errs, len(want))
	}
	for i, w := range want {
		var productErr *messages.ProductError
		if !errors.As(errs[i], &productErr) || productErr.Code() != w.code || productErr.Params()["arg"] != w.arg {
			t.Fatalf("errors[%d] = %v, want %s for %q", i, errs[i], w.code, w.arg)
		}
	}
	if err := ValidateDockerArgs(args, 0); err == nil || err.Error() != errs[0].Error() {
		t.Fatalf("ValidateDockerArgs = %v, want the first of DockerArgErrors %v", err, errs[0])
	}
}

func TestDockerArgHostAccessWarnings(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string // the arg param of each warning
	}{
		{[]string{"--privileged"}, []string{"--privileged"}},
		{[]string{" --privileged "}, []string{"--privileged"}},
		{[]string{"--privileged=true"}, []string{"--privileged=true"}},
		{[]string{"--privileged=false"}, nil},
		{[]string{"--pid=host"}, []string{"--pid=host"}},
		{[]string{"--pid host"}, []string{"--pid host"}},
		{[]string{"--pid", "host"}, []string{"--pid"}},
		{[]string{"--pid=container:abc"}, []string{"--pid=container:abc"}},
		{[]string{"--pid"}, nil},
		{[]string{"--cap-add=SYS_PTRACE", "--ipc=host", "--group-add", "video"}, nil},
		{[]string{"--privileged", "--pid=host"}, []string{"--privileged", "--pid=host"}},
	} {
		warnings := DockerArgHostAccessWarnings(tc.args)
		if len(warnings) != len(tc.want) {
			t.Fatalf("%q: warnings = %+v, want %q", tc.args, warnings, tc.want)
		}
		for i, w := range warnings {
			want := messages.WarningDockerArgSharedPID
			if w.Params["option"] == "--privileged" {
				want = messages.WarningDockerArgPrivileged
			}
			if w.Code != want || w.Params["arg"] != tc.want[i] {
				t.Fatalf("%q: warnings[%d] = %+v, want %s for %q", tc.args, i, w, want, tc.want[i])
			}
		}
	}
}
