package gpu

import "fmt"

// KFD encodes major*10000 + minor*100 + stepping. ROCm prints the
// minor/stepping components in hex, including gfx90a's stepping 10.
// https://github.com/ROCm/rocminfo/blob/amd-staging_deprecated/rocm_agent_enumerator
func amdGFXTarget(version int64) string {
	if version <= 0 {
		return ""
	}
	major, minor, stepping := version/10000, (version/100)%100, version%100
	if major == 0 || minor > 15 || stepping > 15 {
		return ""
	}
	return fmt.Sprintf("gfx%d%x%x", major, minor, stepping)
}

// Match KFD to DRM by render minor, never by directory enumeration order.
func (p *AMDProber) archID(devicePath string) string {
	bdf := p.deviceBDF(devicePath)
	properties, reason := p.matchingKFDProperties(devicePath, bdf)
	if reason != "" {
		return ""
	}
	return amdGFXTarget(properties["gfx_target_version"])
}
