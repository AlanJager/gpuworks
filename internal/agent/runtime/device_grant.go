package runtime

import (
	"strconv"
	"strings"
)

// DeviceAccessAll is read, write and mknod: what a compute device needs.
const DeviceAccessAll = "rwm"

// Device is a host device node the workload gets at the same path.
type Device struct {
	Path string
	// Access is the device-cgroup access asked for (DeviceAccessAll). Empty
	// passes the node bare and leaves access to the runtime's default.
	Access string
}

// DockerValue is the node as the value of docker's --device option. The kube
// runtime's device plugin takes the same spelling.
func (d Device) DockerValue() string {
	if d.Access == "" {
		return d.Path
	}
	return d.Path + ":" + d.Path + ":" + d.Access
}

// DeviceGrant is the accelerator devices a workload is given, stated as
// facts about the host: whose cards, which device nodes, which runtime
// class, which supplementary groups. A device backend fills it in and each
// driver gives it to the container in its own way; it never holds the
// options of one container CLI.
type DeviceGrant struct {
	// Vendor is whose cards these are, by the accelerator package's
	// canonical name (gpu.ResolveInjection writes it). A driver that
	// prepares a container differently per vendor reads it rather than
	// inferring the vendor from Env and DockerArgs. It is empty when no
	// cards were granted and in a workload that was not built from a
	// resolved injection.
	Vendor string
	// RuntimeClass is the container runtime that injects the devices itself
	// (Ascend Docker Runtime), or the plain one a backend insists on.
	RuntimeClass string
	// ToolkitDevices are the host indexes asked of the NVIDIA Container
	// Toolkit, which renumbers them from zero inside the container.
	ToolkitDevices []int
	// Devices are the device nodes handed over, in the backend's order.
	Devices []Device
	// Groups are host group ids the workload's process joins to open them.
	Groups []int
}

// Clone returns a grant that shares no slice with g.
func (g DeviceGrant) Clone() DeviceGrant {
	g.ToolkitDevices = append([]int(nil), g.ToolkitDevices...)
	g.Devices = append([]Device(nil), g.Devices...)
	g.Groups = append([]int(nil), g.Groups...)
	return g
}

// ToolkitDeviceList is ToolkitDevices as the toolkit takes them: comma
// separated.
func (g DeviceGrant) ToolkitDeviceList() string {
	values := make([]string, len(g.ToolkitDevices))
	for i, index := range g.ToolkitDevices {
		values[i] = strconv.Itoa(index)
	}
	return strings.Join(values, ",")
}

// DockerDeviceArgs renders the grant as `docker run` options, for the
// drivers whose CLI takes them (docker, nerdctl). The order is fixed:
// runtime class, toolkit devices, device nodes in the order they were
// granted, then groups.
func (g DeviceGrant) DockerDeviceArgs() []string {
	var args []string
	if g.RuntimeClass != "" {
		args = append(args, "--runtime", g.RuntimeClass)
	}
	if len(g.ToolkitDevices) > 0 {
		// Docker parses --gpus values as CSV. Without the inner quotes,
		// device=0,1 becomes DeviceIDs=[0] plus Count=1 and recent daemons
		// reject the request as "cannot set both Count and DeviceIDs". exec
		// does not add shell quoting for us, so the quote characters are
		// intentional.
		args = append(args, "--gpus", `"device=`+g.ToolkitDeviceList()+`"`)
	}
	for _, device := range g.Devices {
		args = append(args, "--device", device.DockerValue())
	}
	for _, group := range g.Groups {
		args = append(args, "--group-add", strconv.Itoa(group))
	}
	return args
}

// DockerRunArgs is everything the workload asks of `docker run` beyond its
// image and command: the options it carries, then the devices it was
// granted. The granted selection is authoritative over a singleton among the
// workload's own: an image-level `--gpus all` does not remain beside the
// cards the scheduler chose, nor a runtime beside the one the backend names.
func (s WorkloadSpec) DockerRunArgs() []string {
	args := append([]string(nil), s.DockerArgs...)
	if len(s.Grant.ToolkitDevices) > 0 {
		args = WithoutDockerOption(args, "--gpus")
	}
	if s.Grant.RuntimeClass != "" {
		args = WithoutDockerOption(args, "--runtime")
	}
	return append(args, s.Grant.DockerDeviceArgs()...)
}

// WithoutDockerOption returns args without an option that takes a value, in
// either spelling (`--opt value`, `--opt=value`).
func WithoutDockerOption(args []string, option string) []string {
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, option+"=") {
			continue
		}
		if arg == option {
			if index+1 < len(args) {
				index++
			}
			continue
		}
		result = append(result, arg)
	}
	return result
}
