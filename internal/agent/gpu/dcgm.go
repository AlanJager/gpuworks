package gpu

// dcgm.go: optional DCGM SM occupancy probe (plan §4.1). NVIDIA's profiling
// counters (SM_ACTIVE/SM_OCCUPANCY/TENSOR_ACTIVE) are exposed only by
// DCGM/NVML, never by nvidia-smi --query-gpu, so this is a second, separately
// gated CLI probe rather than an extra --query-gpu field. Per plan §4.1 this
// probe defaults to OFF: it issues zero extra commands per tick unless the
// agent is started with --dcgm=auto (which then still requires dcgmi to
// actually be on PATH before it does anything).

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// dcgmLookPath and dcgmRunCommand are package vars (not struct fields) so
// tests can simulate an absent/present dcgmi binary and its output without
// touching PATH or shelling out for real — the same seam style cuda_smoke.go
// uses for its own single external dependency.
var (
	dcgmLookPath   = exec.LookPath
	dcgmRunCommand = func(ctx context.Context, args ...string) (string, error) {
		return QueryVendorCLI(ctx, "dcgmi", args...)
	}
)

// DCGMSample is one GPU's fractional (0..1) profiling counters for one tick.
type DCGMSample struct {
	SMActive, SMOccupancy, TensorActive float64
}

// DCGMProbe runs "dcgmi dmon" once per tick when enabled and available.
type DCGMProbe struct{}

// NewDCGMProbe returns a probe only when mode is explicitly "auto" (case
// insensitive) AND the dcgmi CLI is on PATH; nil for every other mode,
// including "off" and the empty/default value. This probe is opt-in (plan
// §4.1): an agent that never passes --dcgm=auto must never issue an extra
// per-tick command, regardless of what happens to be on its PATH. Returning
// nil rather than a probe that always errors keeps every caller's contract
// simple: disabled/absent DCGM costs zero commands and zero series, never a
// per-tick failure.
func NewDCGMProbe(mode string) *DCGMProbe {
	if !strings.EqualFold(strings.TrimSpace(mode), "auto") {
		return nil
	}
	if _, err := dcgmLookPath("dcgmi"); err != nil {
		return nil
	}
	return &DCGMProbe{}
}

// Sample runs exactly one "dcgmi dmon -c 1 -e 1002,1003,1004" (SM_ACTIVE,
// SM_OCCUPANCY, TENSOR_ACTIVE) and parses its per-GPU fractions, keyed by the
// same GPU index nvidia-smi reports so the collector can merge them into the
// same per-GPU sample without a second identity lookup.
func (p *DCGMProbe) Sample(ctx context.Context) (map[int]DCGMSample, error) {
	out, err := dcgmRunCommand(ctx, "dmon", "-c", "1", "-e", "1002,1003,1004")
	if err != nil {
		return nil, err
	}
	return parseDCGMDmon(out), nil
}

// parseDCGMDmon parses "dcgmi dmon" plain-text output: comment/header lines
// start with '#', each data row is "GPU <index> <smact> <smocc> <tenso>" in
// the field order requested. A row DCGM could not sample prints "N/A" and is
// skipped rather than reported as zero, the same convention the nvidia-smi
// parser uses for an unsupported field.
func parseDCGMDmon(out string) map[int]DCGMSample {
	result := map[int]DCGMSample{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[0], "GPU") {
			continue
		}
		idx, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		smActive, aErr := strconv.ParseFloat(fields[2], 64)
		smOcc, oErr := strconv.ParseFloat(fields[3], 64)
		tensor, tErr := strconv.ParseFloat(fields[4], 64)
		if aErr != nil || oErr != nil || tErr != nil {
			continue
		}
		result[idx] = DCGMSample{SMActive: smActive, SMOccupancy: smOcc, TensorActive: tensor}
	}
	return result
}
