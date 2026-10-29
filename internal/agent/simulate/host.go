package simulate

import (
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/hostsim"
)

// metricsInterval keeps the experience environment's metrics small: six
// nodes at the real 10s would triple what its monitoring stores.
const metricsInterval = 30 * time.Second

// Host is the policy of spec's machine (hostsim): it describes the machine
// in place of the real host and lets a tunnel reach only the engines the
// node's instances claimed. The agent confines its files to its models
// directory (hostsim.Host.Dir).
func (p *Prober) Host() *hostsim.Host {
	h := &hostsim.Host{
		MayDial:         p.MayDial,
		Describe:        p.DescribeHost,
		Samples:         p.HostSamples,
		Listeners:       p.EnginePortListeners,
		MetricsInterval: metricsInterval,
	}
	if accelerator.NormalizeVendor(p.spec.Vendor) == accelerator.VendorNVIDIA {
		h.Libvgpu = writeLibvgpuPlaceholder
	}
	return h
}

// writeLibvgpuPlaceholder stands in for the NVIDIA dGPU library, which the
// simulated runtime mounts but never loads.
func writeLibvgpuPlaceholder(path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte("simulated libvgpu\n"), 0o644)
}
