package accelerator

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// HostRuntime is observed host information, never inferred from the GPU SKU.
type HostRuntime struct{ OS, Arch, Driver string }

// The runtime drivers a vendor can list, by the names nodes report them.
const (
	RuntimeDocker  = proto.RuntimeDriverDocker
	RuntimeNerdctl = proto.RuntimeDriverNerdctl
	RuntimeUdocker = proto.RuntimeDriverUdocker
	RuntimeKube    = proto.RuntimeDriverKube
	RuntimeHost    = proto.RuntimeDriverHost
)

// Runtimes lists every runtime driver a vendor can be declared on.
func Runtimes() []string {
	return []string{RuntimeDocker, RuntimeNerdctl, RuntimeUdocker, RuntimeKube, RuntimeHost}
}

func (s vendorSpec) runsOn(driver string) bool {
	for _, own := range s.runtimes {
		if own == driver {
			return true
		}
	}
	return false
}

// RuntimeMismatch describes observed facts that fail a target's host contract.
type RuntimeMismatch struct {
	MissingFacts             bool
	Vendor                   string
	Actual                   HostRuntime
	RequiredOS, RequiredArch string
	RequiredDrivers          []string
}

func (m *RuntimeMismatch) Params() map[string]any {
	return map[string]any{"missing_facts": m.MissingFacts, "vendor": VendorBrand(m.Vendor), "os": m.Actual.OS, "arch": m.Actual.Arch, "runtime": m.Actual.Driver, "required_os": m.RequiredOS, "required_arch": m.RequiredArch, "required_runtime": strings.Join(m.RequiredDrivers, "/")}
}

// RuntimeMismatchFor checks what a node reports against what the vendor's
// definition declares, for a vendor that requires a particular operating
// system or architecture: those must match and the runtime driver must be
// one it lists. A name that is no vendor's has no definition to check
// against.
func RuntimeMismatchFor(vendor string, actual HostRuntime) *RuntimeMismatch {
	spec, known := vendorSpecOf(vendor)
	if !known {
		return nil
	}
	hostOS, hostArch := strings.TrimSpace(actual.OS), strings.TrimSpace(actual.Arch)
	driver := strings.ToLower(strings.TrimSpace(actual.Driver))
	osOK := spec.hostOS == "" || strings.EqualFold(spec.hostOS, hostOS)
	archOK := spec.hostArch == "" || NormalizeArch(spec.hostArch) == NormalizeArch(hostArch)
	// The runtime is judged only for a vendor that requires a particular
	// host.
	driverOK := spec.runsOn(driver) || (spec.hostOS == "" && spec.hostArch == "")
	if osOK && archOK && driverOK {
		return nil
	}
	// A fact the node has not reported is missing, not wrong; the refusal
	// says so only when nothing it did report is wrong.
	wrong := (!osOK && hostOS != "") || (!archOK && hostArch != "") || (!driverOK && driver != "")
	return &RuntimeMismatch{MissingFacts: !wrong, Vendor: vendor, Actual: actual, RequiredOS: spec.hostOS, RequiredArch: spec.hostArch, RequiredDrivers: append([]string(nil), spec.runtimes...)}
}

// RuntimeMismatchParams is the serialization adapter retained for consumers
// that expose the established refusal vocabulary.
func RuntimeMismatchParams(vendor string, actual HostRuntime) map[string]any {
	if m := RuntimeMismatchFor(vendor, actual); m != nil {
		return m.Params()
	}
	return nil
}
