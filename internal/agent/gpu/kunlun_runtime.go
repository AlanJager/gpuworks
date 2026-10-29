package gpu

import "context"

var _ TopologyAwareInjector = (*KunlunProber)(nil)

// The launch extension carries cancellation without adding transport devices
// to local TP. Cross-node transport is added by a separate capability patch.
func (p *KunlunProber) RuntimeInjectionForLaunch(ctx context.Context, indexes []int, driver string, nNodes int, _ []string) RuntimeInjection {
	if len(indexes) == 0 {
		return RuntimeInjection{}
	}
	if nNodes > 1 {
		return RuntimeInjection{UnsupportedReason: "Kunlun multi-node runtime is unavailable"}
	}
	return p.runtimeInjectionForDriver(ctx, indexes, driver)
}
