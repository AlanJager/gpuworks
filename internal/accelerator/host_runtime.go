package accelerator

import "strings"

// HostRuntime is observed host information, never inferred from the GPU SKU.
type HostRuntime struct{ OS, Arch, Driver string }

type hostRequirements struct {
	os, arch string
	drivers  []string
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

// RuntimeMismatchFor checks observed runtime facts without encoding policy in
// a formatting map. Unknown targets impose no extra runtime requirements.
// Unknown targets have no extra constraint at this layer.
func RuntimeMismatchFor(vendor string, actual HostRuntime) *RuntimeMismatch {
	r := targetConstraints[NormalizeVendor(vendor)].runtime
	osMissing := r.os != "" && strings.TrimSpace(actual.OS) == ""
	archMissing := r.arch != "" && strings.TrimSpace(actual.Arch) == ""
	driverMissing := len(r.drivers) != 0 && strings.TrimSpace(actual.Driver) == ""
	knownMismatch := (r.os != "" && !osMissing && !strings.EqualFold(r.os, strings.TrimSpace(actual.OS))) ||
		(r.arch != "" && !archMissing && NormalizeArch(r.arch) != NormalizeArch(actual.Arch))
	driverOK := len(r.drivers) == 0
	for _, driver := range r.drivers {
		if strings.EqualFold(driver, strings.TrimSpace(actual.Driver)) {
			driverOK = true
		}
	}
	if (r.os == "" || strings.EqualFold(r.os, strings.TrimSpace(actual.OS))) &&
		(r.arch == "" || NormalizeArch(r.arch) == NormalizeArch(actual.Arch)) && driverOK {
		return nil
	}
	knownMismatch = knownMismatch || (!driverMissing && !driverOK)
	return &RuntimeMismatch{MissingFacts: (osMissing || archMissing || driverMissing) && !knownMismatch, Vendor: vendor, Actual: actual, RequiredOS: r.os, RequiredArch: r.arch, RequiredDrivers: append([]string(nil), r.drivers...)}
}

// RuntimeMismatchParams is the serialization adapter retained for consumers
// that expose the established refusal vocabulary.
func RuntimeMismatchParams(vendor string, actual HostRuntime) map[string]any {
	if m := RuntimeMismatchFor(vendor, actual); m != nil {
		return m.Params()
	}
	return nil
}
