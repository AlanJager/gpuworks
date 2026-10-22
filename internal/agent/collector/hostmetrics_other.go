//go:build !linux && !darwin

// Non-Linux stub: there is no /proc, so host CPU/disk/network rate sampling
// is simply unavailable, mirroring how sysinfo_darwin.go leaves the
// Linux-only host facts zero rather than faking them.
package collector

import "gpuworks.ai/gpuworks/internal/proto"

// hostSampler on a non-Linux build always has nothing to report. The type
// still exists here so collector.go stays free of build tags: New always
// constructs one and hostSamples always calls sample.
type hostSampler struct{}

func newHostSampler() *hostSampler { return &hostSampler{} }

func (h *hostSampler) sample(node, modelsRoot string, now int64) []proto.MetricSample { return nil }
