package gpu

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

const (
	cudaGradedOKMarker          = "CUDA_RUNTIME_SMOKE_OK"
	cudaGradedCardMarker        = "CUDA_RUNTIME_SMOKE_CARD_FAILED:"
	cudaGradedUnavailableMarker = "CUDA_RUNTIME_SMOKE_UNAVAILABLE:"
)

// ContainerSmokeVerdict grades what the runtime check printed inside a
// container (accelerator.ContainerSmokeScript) and how the container exited.
// Only the script's own card marker blames the card: an image without the
// runtime or without python, a container that crashed, or output this agent
// does not recognize is reported as the host's (HostRuntimeUnavailableError).
func ContainerSmokeVerdict(vendor string, out string, exitCode int) error {
	var exitErr error
	if exitCode != 0 {
		exitErr = fmt.Errorf("container exited with code %d", exitCode)
	}
	switch accelerator.NormalizeVendor(vendor) {
	case accelerator.VendorAscend:
		return ascendSmokeVerdict([]byte(out), exitErr)
	case accelerator.VendorPPU:
		return cudaGradedVerdict(out, exitErr)
	}
	return &HostRuntimeUnavailableError{Reason: "no container runtime check for vendor " + vendor}
}

func cudaGradedVerdict(out string, exitErr error) error {
	if detail, ok := ascendSmokeMarkerDetail(out, cudaGradedUnavailableMarker); ok {
		return &HostRuntimeUnavailableError{Reason: "the runtime in the image could not be used: " + detail}
	}
	if detail, ok := ascendSmokeMarkerDetail(out, cudaGradedCardMarker); ok {
		return fmt.Errorf("runtime smoke failed: %s", detail)
	}
	if exitErr == nil && strings.Contains(out, cudaGradedOKMarker) {
		return nil
	}
	detail := strings.TrimSpace(out)
	if detail == "" && exitErr != nil {
		detail = exitErr.Error()
	}
	if detail == "" {
		detail = "the check printed nothing"
	}
	return &HostRuntimeUnavailableError{Reason: "the runtime check in the container did not reach the card: " + detail}
}
