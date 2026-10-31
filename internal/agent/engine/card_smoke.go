package engine

import (
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// BuildCardSmokeWorkload is the container the per-card runtime check runs in
// on a host whose runtime lives only in images: image (a runner image already
// on the node), the one card at gpuIndex injected exactly as a real instance
// gets it, and entrypoint as its command. Nothing else of an instance's is
// added — no model, no ports, no partition.
func (e *Engine) BuildCardSmokeWorkload(image string, gpuIndex int, name string, entrypoint []string) (agentruntime.WorkloadSpec, error) {
	ws := toWorkloadSpec(proto.InstanceDesiredState{UUID: name, Image: image, GPUIndexes: []int{gpuIndex}}, name)
	if err := e.applyDeviceInjection(&ws, ws.GPUIndexes); err != nil {
		return agentruntime.WorkloadSpec{}, err
	}
	ws.Entrypoint = entrypoint
	ws.Cmd = nil
	return ws, nil
}

// DesiredImages are the images of the instances this agent is told to run.
func (e *Engine) DesiredImages() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	images := make([]string, 0, len(e.desired))
	for _, spec := range e.desired {
		if spec.Image != "" {
			images = append(images, spec.Image)
		}
	}
	return images
}
