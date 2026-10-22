package accelerator

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DriverRequirement returns the concise driver requirement that can be
// derived from a framework version. It is presentation metadata only; image
// selection remains advisory and never rejects an explicit user choice.
func DriverRequirement(framework, frameworkVersion string) string {
	framework = strings.ToLower(strings.TrimSpace(framework))
	switch framework {
	case "cuda":
		if minimum, ok := cudaMinimumDriver(frameworkVersion); ok {
			return fmt.Sprintf("CUDA driver >= %d", minimum)
		}
	case "cann":
		parts := numericParts(frameworkVersion)
		if len(parts) >= 2 && parts[0] == 9 && parts[1] >= 1 {
			return "Ascend HDK 25.2 - 26.1"
		}
		if len(parts) >= 1 && (parts[0] == 8 || parts[0] == 9) {
			return "Ascend HDK 25.0 - 26.1"
		}
	case "dtk":
		version := strings.ToLower(strings.TrimSpace(frameworkVersion))
		switch {
		case strings.HasPrefix(version, "26.04.1"):
			return "DTK driver = 6.4.0-V1.7.0"
		case strings.HasPrefix(version, "26.04"):
			return "DTK driver >= 6.3.30-V1.4.1a"
		case strings.HasPrefix(version, "25.04.2"):
			return "DTK driver >= 6.3.16-V1.1.0a"
		case strings.HasPrefix(version, "25.04.1"):
			return "DTK driver >= 6.3.6-V1.9.0c"
		case strings.HasPrefix(version, "25.04"):
			return "DTK driver >= 5.7.1-V1.7.0"
		case strings.HasPrefix(version, "24.04"):
			return "DTK driver 5.7.1-V1.0 - 5.7.1-V1.6.3"
		}
	case "metal":
		// An environment package's framework version is the oldest macOS
		// its wheels were built for; Metal ships with the OS.
		if len(numericParts(frameworkVersion)) > 0 {
			return "macOS >= " + strings.TrimSpace(frameworkVersion)
		}
	case "hggc":
		parts := numericParts(frameworkVersion)
		if len(parts) >= 2 && parts[0] == 13 {
			return "KMD >= 1.6"
		}
		if len(parts) >= 2 && parts[0] == 12 && parts[1] >= 9 {
			return "KMD >= 1.4"
		}
		if len(parts) >= 2 && parts[0] == 12 && parts[1] == 3 {
			return "KMD >= 1.3.2"
		}
	}
	return ""
}

// CompatLevel is deliberately advisory. An empty level means that the
// supplied driver is at least as new as the known minimum. Callers must not
// use this result to reject an image.
type CompatLevel string

const (
	CompatWarning CompatLevel = "warning"
	CompatUnknown CompatLevel = "unknown"
)

// CheckDriverCompat compares a node driver string with the framework runtime
// baked into an image. It returns a warning/unknown level and a stable reason;
// a blank level means no advisory was necessary. Driver values are expected to
// be canonical, but the parser still accepts legacy diagnostic strings.
func CheckDriverCompat(driver, framework, frameworkVersion string) (CompatLevel, string) {
	driver = strings.TrimSpace(driver)
	framework = strings.ToLower(strings.TrimSpace(framework))
	frameworkVersion = strings.TrimSpace(frameworkVersion)
	if driver == "" || frameworkVersion == "" {
		return CompatUnknown, "driver or framework version is unavailable"
	}
	switch framework {
	case "cuda":
		driverVersion, ok := firstVersion(driver)
		if !ok {
			return CompatUnknown, "driver version is not parseable"
		}
		minimum, ok := cudaMinimumDriver(frameworkVersion)
		if !ok {
			return CompatUnknown, "CUDA framework version is not recognized"
		}
		if len(driverVersion) == 0 || driverVersion[0] < minimum {
			return CompatWarning, "driver is older than the minimum CUDA driver"
		}
		// Later NVIDIA drivers remain backward compatible with older CUDA
		// applications. The upper bounds shown in NVIDIA's minor-version
		// compatibility table are not an incompatibility ceiling.
		return "", ""
	case "cann":
		return checkCANN(driver, frameworkVersion)
	case "dtk":
		return checkDTK(driver, frameworkVersion)
	case "hggc":
		return checkHGGC(driver, frameworkVersion)
	case "metal":
		return checkMetal(driver, frameworkVersion)
	default:
		return CompatUnknown, "framework compatibility is not known"
	}
}

func cudaMinimumDriver(version string) (int, bool) {
	parts := numericParts(version)
	if len(parts) == 0 {
		return 0, false
	}
	switch parts[0] {
	case 11:
		return 450, true
	case 12:
		return 525, true
	case 13:
		return 580, true
	default:
		return 0, false
	}
}

func checkCANN(driver, frameworkVersion string) (CompatLevel, string) {
	driverParts, ok := firstVersion(driver)
	if !ok {
		return CompatUnknown, "Ascend HDK version is not parseable"
	}
	frameworkParts := numericParts(frameworkVersion)
	if len(frameworkParts) < 2 {
		return CompatUnknown, "CANN framework version is not parseable"
	}
	// The supplied CANN 9.1 matrix requires HDK 25.2.x or newer. CANN
	// 8.5/9.0 are documented for 25.0.x through 26.1.x. Keep the matrix
	// advisory because vendor releases can add combinations over time.
	min := []int{25, 0}
	if frameworkParts[0] == 9 && frameworkParts[1] >= 1 {
		min = []int{25, 2}
	}
	if compareParts(driverParts, min) < 0 {
		return CompatWarning, "Ascend HDK is older than the known CANN minimum"
	}
	if compareParts(driverParts, []int{26, 1}) > 0 {
		return CompatWarning, "Ascend HDK is newer than the known CANN matrix"
	}
	return "", ""
}

func checkDTK(driver, frameworkVersion string) (CompatLevel, string) {
	driverParts, ok := normalizedDTKVersion(driver)
	if !ok {
		return CompatUnknown, "DTK driver version is not parseable"
	}
	framework := strings.ToLower(strings.TrimSpace(frameworkVersion))
	// The detailed DTK matrix is intentionally represented as advisory bounds.
	// Unknown releases stay Unknown instead of being guessed from a major.
	switch {
	case strings.HasPrefix(framework, "26.04.1"):
		if compareParts(driverParts, []int{6, 4, 0, 1, 7, 0}) != 0 {
			return CompatWarning, "DTK 26.04.1 expects driver 6.4.0-V1.7.0"
		}
	case strings.HasPrefix(framework, "26.04"):
		if compareParts(driverParts, []int{6, 3, 30, 1, 4, 1}) < 0 {
			return CompatWarning, "DTK 26.04 requires driver 6.3.30-V1.4.1a or newer"
		}
	case strings.HasPrefix(framework, "25.04.2"):
		if compareParts(driverParts, []int{6, 3, 16, 1, 1, 0}) < 0 {
			return CompatWarning, "DTK 25.04.2 requires driver 6.3.16-V1.1.0a or newer"
		}
	case strings.HasPrefix(framework, "25.04.1"):
		if compareParts(driverParts, []int{6, 3, 6, 1, 9, 0}) < 0 {
			return CompatWarning, "DTK 25.04.1 requires driver 6.3.6-V1.9.0c or newer"
		}
	case strings.HasPrefix(framework, "25.04"):
		if compareParts(driverParts, []int{5, 7, 1, 1, 7, 0}) < 0 {
			return CompatWarning, "DTK 25.04 requires driver 5.7.1-V1.7.0 or newer"
		}
	case strings.HasPrefix(framework, "24.04"):
		if compareParts(driverParts, []int{5, 7, 1, 1, 0}) < 0 || compareParts(driverParts, []int{5, 7, 1, 1, 6, 3}) > 0 {
			return CompatWarning, "DTK 24.04 requires a driver in the 5.7.1-V1.0 through V1.6.3 range"
		}
	default:
		return CompatUnknown, "DTK framework version is not recognized"
	}
	return "", ""
}

func checkHGGC(driver, frameworkVersion string) (CompatLevel, string) {
	parts, ok := firstVersion(driver)
	if !ok || len(numericParts(frameworkVersion)) == 0 {
		return CompatUnknown, "HGGC or KMD version is unavailable"
	}
	// HGGC compatibility is a hardware + SDK + KMD + firmware tuple. A raw
	// driver string cannot prove that tuple, so known version pairs remain
	// advisory and never produce a positive admission decision.
	fw := numericParts(frameworkVersion)
	if fw[0] == 13 && compareParts(parts, []int{1, 6}) < 0 {
		return CompatWarning, "HGGC 13 requires a known-compatible KMD"
	}
	if fw[0] == 12 && len(fw) > 1 && fw[1] >= 9 && compareParts(parts, []int{1, 4}) < 0 {
		return CompatWarning, "HGGC 12.9 requires a known-compatible KMD"
	}
	if fw[0] == 12 && len(fw) > 1 && fw[1] == 3 && compareParts(parts, []int{1, 3, 2}) < 0 {
		return CompatWarning, "HGGC 12.3 requires a known-compatible KMD"
	}
	return CompatUnknown, "HGGC compatibility requires hardware, SDK, KMD and firmware facts"
}

var versionRE = regexp.MustCompile(`\d+(?:[.]\d+)*(?:[.](?i:[a-z][a-z0-9]*))?`)
var labeledVersionRE = regexp.MustCompile(`(?i)(?:driver\s+version|version)\s*:\s*([0-9]+(?:\.[0-9]+)*(?:\.[a-z][a-z0-9]*)?)`)

// NormalizeDriverVersion returns the concise version shown to operators while
// leaving the raw node report available to compatibility checks. Vendor tools
// often print the same value twice, for example npu-smi's header and Version
// field; only the labeled version is useful in the node list.
func NormalizeDriverVersion(vendor, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if match := labeledVersionRE.FindStringSubmatch(raw); len(match) > 1 {
		return normalizeVersionText(match[1])
	}
	if match := versionRE.FindString(raw); match != "" {
		return normalizeVersionText(match)
	}
	return raw
}

func normalizeVersionText(value string) string {
	parts := strings.Split(value, ".")
	for i, part := range parts {
		if strings.EqualFold(part, "rc1") {
			parts[i] = "RC1"
		}
	}
	return strings.Join(parts, ".")
}

func firstVersion(value string) ([]int, bool) {
	match := versionRE.FindString(value)
	if match == "" {
		return nil, false
	}
	return numericParts(match), true
}

func normalizedDTKVersion(value string) ([]int, bool) {
	match := versionRE.FindString(value)
	if match == "" {
		return nil, false
	}
	parts := numericParts(match)
	// DTK strings include a second component after V, for example
	// 6.4.0-V1.7.0. The regexp sees only the first component; append the V
	// suffix so comparisons retain the release ordering.
	lower := strings.ToLower(value)
	if idx := strings.Index(lower, "v"); idx >= 0 {
		if suffix, ok := firstVersion(value[idx+1:]); ok {
			parts = append(parts, suffix...)
		}
	}
	return parts, true
}

func numericParts(value string) []int {
	parts := strings.FieldsFunc(strings.TrimSpace(strings.TrimPrefix(strings.ToLower(value), "v")), func(r rune) bool {
		return r == '.' || r == '-' || r == '+' || r == '_' || r == ' '
	})
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

func compareParts(a, b []int) int {
	max := len(a)
	if len(b) > max {
		max = len(b)
	}
	for i := 0; i < max; i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

// MinimumMacOSMajor is the oldest macOS an Apple silicon node is supported on
// (ADR 0044): the release vllm-metal and the environment packages build for.
// A package may require a newer one; checkMetal judges that per package.
const MinimumMacOSMajor = 15

// checkMetal compares the node's macOS version (the driver an Apple silicon
// node reports) with the oldest macOS an environment package supports.
func checkMetal(driver, frameworkVersion string) (CompatLevel, string) {
	have, ok := firstVersion(driver)
	if !ok {
		return CompatUnknown, "macOS version is not parseable"
	}
	need := numericParts(frameworkVersion)
	if len(need) == 0 {
		return CompatUnknown, "minimum macOS version is not parseable"
	}
	if compareParts(have, need) < 0 {
		return CompatWarning, "macOS is older than the environment package requires"
	}
	return "", ""
}
