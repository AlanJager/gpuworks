package deploy

import (
	_ "embed"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

const ascendV030RC2A3CompatMarker = "# GPUWorks: vLLM-Ascend v0.30.0rc2-a3 compatibility\n"
const ascendV030RC2A3V2CompatMarker = "# GPUWorks: vLLM-Ascend v0.30.0rc2-a3 V2 compatibility\n"

//go:embed assets/ascend-v0.30.0rc2-a3-compat.sh
var ascendV030RC2A3CompatScript string

//go:embed assets/ascend-v0.30.0rc2-a3-v2-compat.sh
var ascendV030RC2A3V2CompatScript string

type ascendRuntimeCompat uint8

const (
	ascendRuntimeCompatNone ascendRuntimeCompat = iota
	ascendRuntimeCompatV1
	ascendRuntimeCompatV2
)

// ascendRuntimeCompatFor selects the image-specific startup fixes only for the
// PRoot runtime. The fixed image's Ascend platform patch replaces upstream's
// automatic runner defaults: unset means V1, and an explicitly set integer is
// true exactly when bool(int(value)) would be true.
func ascendRuntimeCompatFor(engineName, image, runtimeDriver string, env map[string]string) ascendRuntimeCompat {
	image = strings.TrimSpace(image)
	if engineName != EngineVLLM || !strings.EqualFold(strings.TrimSpace(runtimeDriver), proto.RuntimePreferenceUdocker) {
		return ascendRuntimeCompatNone
	}
	if !strings.Contains(image, "vllm-ascend:v0.30.0rc2-a3") {
		return ascendRuntimeCompatNone
	}
	value, err := strconv.Atoi(strings.TrimSpace(env["VLLM_USE_V2_MODEL_RUNNER"]))
	if err == nil && value != 0 {
		return ascendRuntimeCompatV2
	}
	return ascendRuntimeCompatV1
}

// appendAscendRuntimeCompatibilityScript prepends the fixes to any preparation
// the launch already owns. The scripts patch only the fixed image's known file
// layout and refuse an unexpected layout instead of guessing.
func appendAscendRuntimeCompatibilityScript(compat ascendRuntimeCompat, existing string) string {
	var marker, script string
	switch compat {
	case ascendRuntimeCompatV1:
		marker, script = ascendV030RC2A3CompatMarker, ascendV030RC2A3CompatScript
	case ascendRuntimeCompatV2:
		marker, script = ascendV030RC2A3V2CompatMarker, ascendV030RC2A3V2CompatScript
	default:
		return existing
	}
	if strings.Contains(existing, marker) {
		return existing
	}
	script = marker + script
	if strings.TrimSpace(existing) == "" {
		return script
	}
	return script + "\n" + existing
}
