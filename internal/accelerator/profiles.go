package accelerator

import (
	"fmt"
	"strings"
)

// PartitionProfile records observed compatibility combinations for warnings
// and documentation. It never selects a runtime asset: assets are
// architecture-specific, while support is established by behavior probes.
type PartitionProfile struct {
	Backend              string
	Vendor               string
	Framework            string
	VersionPrefix        string
	VersionOptional      bool
	Architectures        []string
	DisplayCompatibility string
	Verified             bool
	ChipFamilies         []string
}

// PartitionProfiles returns every backend's build profiles in registry
// order. Backends without profiles contribute nothing.
func PartitionProfiles() []PartitionProfile {
	var out []PartitionProfile
	for _, backend := range All() {
		out = append(out, backend.Profiles()...)
	}
	return out
}

// SupportsFramework reports whether any build profile of the backend targets
// the given image framework, so callers can warn only where the backend
// actually serves that framework.
func SupportsFramework(b Backend, framework string) bool {
	for _, profile := range b.Profiles() {
		if strings.EqualFold(strings.TrimSpace(framework), profile.Framework) {
			return true
		}
	}
	return false
}

// MatchProfile resolves the build profile for an observed host combination.
// A backend without profiles — whether unknown to the registry or simply
// built for none — yields the same "unknown partition backend" error.
func MatchProfile(backend, framework, version, arch string) (PartitionProfile, error) {
	var rows []PartitionProfile
	if b, ok := Lookup(backend); ok {
		rows = b.Profiles()
	}
	matchedBackend := false
	var last PartitionProfile
	for _, profile := range rows {
		matchedBackend = true
		last = profile
		if !strings.EqualFold(strings.TrimSpace(framework), profile.Framework) {
			continue
		}
		canonicalArch := NormalizeArch(arch)
		archOK := false
		for _, supported := range profile.Architectures {
			archOK = archOK || canonicalArch == supported
		}
		if !archOK {
			continue
		}
		if strings.TrimSpace(version) == "" && profile.VersionOptional {
			return profile, nil
		}
		if versionPrefix(version, profile.VersionPrefix) {
			return profile, nil
		}
	}
	if !matchedBackend {
		return PartitionProfile{}, fmt.Errorf("unknown partition backend %q", backend)
	}
	if !strings.EqualFold(strings.TrimSpace(framework), last.Framework) {
		return last, fmt.Errorf("detected framework %s, want %s", framework, last.Framework)
	}
	return last, fmt.Errorf("detected %s %s × %s, outside the supported profile", last.Framework, version, NormalizeArch(arch))
}

// ProfileCompatibility converts the historical build-combination table into
// advisory metadata. Empty host versions are expected on driver-only nodes and
// therefore never produce a warning or an admission failure.
func ProfileCompatibility(backend, framework, version, arch string) (PartitionProfile, string) {
	if strings.TrimSpace(version) == "" {
		return PartitionProfile{}, ""
	}
	profile, err := MatchProfile(backend, framework, version, arch)
	if err != nil {
		return profile, err.Error()
	}
	return profile, ""
}

func versionPrefix(version, prefix string) bool {
	v := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(version), "v"))
	p := strings.ToLower(strings.TrimSpace(prefix))
	return v == p || strings.HasPrefix(v, p+".") || strings.HasPrefix(v, p+"-")
}
