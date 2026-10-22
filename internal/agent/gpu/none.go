package gpu

import (
	"context"

	"gpuworks.ai/gpuworks/internal/proto"
)

// NoAccelerator is the prober of a host that shows no sign of any
// accelerator: no backend's CLI answers, none is installed and no vendor's
// device files exist. Its vendor is empty, so the node is reported as having
// no accelerator rather than as an NVIDIA node with no cards.
type NoAccelerator struct{}

const noAcceleratorReason = "no accelerator was detected on this node"

func (NoAccelerator) Vendor() string                                 { return "" }
func (NoAccelerator) Available() bool                                { return false }
func (NoAccelerator) Probe(context.Context) ([]proto.GPUInfo, error) { return nil, nil }
func (NoAccelerator) DriverVersion(context.Context) string           { return "" }
func (NoAccelerator) Topology(context.Context) string                { return "" }
func (NoAccelerator) Metrics(context.Context) ([]Metric, error)      { return nil, nil }

// RuntimeInjection exposes nothing; a workload that asks for devices is told
// there are none rather than started without them.
func (NoAccelerator) RuntimeInjection(indexes []int) RuntimeInjection {
	if len(indexes) == 0 {
		return RuntimeInjection{}
	}
	return RuntimeInjection{UnsupportedReason: noAcceleratorReason}
}
