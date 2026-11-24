package runtime

import (
	"reflect"
	"testing"
)

// The rendering is what the docker and nerdctl CLIs receive, so its spelling
// and order are a contract: the runtime class first, the toolkit request
// quoted the way dockerd's CSV parser needs, each device node in the order
// it was granted, and the groups last.
func TestDockerDeviceArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec WorkloadSpec
		want []string
	}{
		{"nothing granted", WorkloadSpec{Env: map[string]string{"X": "1"}, Grant: DeviceGrant{Vendor: "nvidia"}}, nil},
		{"runtime class", WorkloadSpec{Grant: DeviceGrant{RuntimeClass: "ascend"}}, []string{"--runtime", "ascend"}},
		{"toolkit devices", WorkloadSpec{Grant: DeviceGrant{ToolkitDevices: []int{2, 4}}}, []string{"--gpus", `"device=2,4"`}},
		{"one toolkit device", WorkloadSpec{Grant: DeviceGrant{ToolkitDevices: []int{0}}}, []string{"--gpus", `"device=0"`}},
		{
			"device nodes keep their order and their access",
			WorkloadSpec{Grant: DeviceGrant{Devices: []Device{{Path: "/dev/b", Access: "rwm"}, {Path: "/dev/a"}}}},
			[]string{"--device", "/dev/b:/dev/b:rwm", "--device", "/dev/a"},
		},
		{
			"everything",
			WorkloadSpec{Grant: DeviceGrant{RuntimeClass: "runc",
				Devices: []Device{{Path: "/dev/xpu1", Access: "rwm"}, {Path: "/dev/xdrdrv"}},
				Groups:  []int{39, 998}},
			},
			[]string{"--runtime", "runc", "--device", "/dev/xpu1:/dev/xpu1:rwm", "--device", "/dev/xdrdrv", "--group-add", "39", "--group-add", "998"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.Grant.DockerDeviceArgs(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DockerDeviceArgs() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The workload's own options come first and are not touched: a granted
// device is added after them, never merged into them.
func TestDockerRunArgsKeepsTheWorkloadsOptionsFirstAndUnchanged(t *testing.T) {
	own := []string{"--ipc=host", "--device", "/dev/infiniband"}
	spec := WorkloadSpec{DockerArgs: own, Grant: DeviceGrant{Devices: []Device{{Path: "/dev/card0", Access: "rwm"}}}}
	want := []string{"--ipc=host", "--device", "/dev/infiniband", "--device", "/dev/card0:/dev/card0:rwm"}
	if got := spec.DockerRunArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DockerRunArgs() = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(spec.DockerArgs, []string{"--ipc=host", "--device", "/dev/infiniband"}) || len(own) != 3 {
		t.Fatalf("the workload's own options were changed: %q", spec.DockerArgs)
	}
}

// A granted runtime class or toolkit request replaces the workload's own, in
// either spelling of the option; without one the workload's own stay.
func TestDockerRunArgsReplacesTheWorkloadsOwnSingletons(t *testing.T) {
	own := []string{"--ipc=host", "--gpus", "all", "--gpus=all", "--runtime", "custom", "--privileged"}
	for _, tc := range []struct {
		name  string
		grant DeviceGrant
		want  []string
	}{
		{"nothing granted", DeviceGrant{}, own},
		{"toolkit devices", DeviceGrant{ToolkitDevices: []int{2, 4}}, []string{"--ipc=host", "--runtime", "custom", "--privileged", "--gpus", `"device=2,4"`}},
		{"runtime class", DeviceGrant{RuntimeClass: "ascend"}, []string{"--ipc=host", "--gpus", "all", "--gpus=all", "--privileged", "--runtime", "ascend"}},
		{"device nodes only", DeviceGrant{Devices: []Device{{Path: "/dev/kfd"}}}, append(append([]string(nil), own...), "--device", "/dev/kfd")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := WorkloadSpec{DockerArgs: append([]string(nil), own...), Grant: tc.grant}
			if got := spec.DockerRunArgs(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DockerRunArgs() = %q, want %q", got, tc.want)
			}
			if !reflect.DeepEqual(spec.DockerArgs, own) {
				t.Fatalf("the workload's own options were changed: %q", spec.DockerArgs)
			}
		})
	}
}
