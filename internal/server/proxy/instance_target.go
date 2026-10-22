package proxy

import (
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// instanceHeader pins a request to one instance of the deployment it names.
// It is honoured only on the authenticated internal entry (the playground),
// never on /v1: a client holding an API key must not bypass load balancing or
// a PD deployment's two-stage handoff (plan D9). On a PD deployment a pinned
// request goes to that instance alone, which is not an end-to-end result.
const instanceHeader = "X-GPUWorks-Instance"

// pinnedTarget resolves the pinned instance of a deployment serving model.
// ok is false when the request pins nothing; status is nonzero when it pins
// an instance that cannot take it.
func (p *Proxy) pinnedTarget(r *http.Request, modelName string) (dep *store.Deployment, tgt target, ok bool, status int, message string) {
	uuid := strings.TrimSpace(r.Header.Get(instanceHeader))
	r.Header.Del(instanceHeader)
	if uuid == "" {
		return nil, target{}, false, 0, ""
	}
	inst, err := p.store.InstanceByUUID(uuid)
	if err != nil {
		return nil, target{}, true, http.StatusNotFound, "instance " + uuid + " does not exist"
	}
	dep, err = p.store.GetDeployment(inst.DeploymentID)
	if err != nil || dep.ServedName != modelName {
		return nil, target{}, true, http.StatusNotFound, "instance " + uuid + " does not serve " + modelName
	}
	if inst.State != proto.InstRunning || inst.SleepLevel > 0 || inst.Headless || inst.APIPort <= 0 {
		return dep, target{}, true, http.StatusServiceUnavailable, "instance " + uuid + " is not awake and running"
	}
	node, err := p.store.NodeByID(inst.NodeID)
	if err != nil || !p.hub.Online(node.UUID) {
		return dep, target{}, true, http.StatusServiceUnavailable, "instance " + uuid + " is offline"
	}
	return dep, target{nodeUUID: node.UUID, port: inst.APIPort, modelName: instanceModelName(dep, inst), instanceUUID: inst.UUID}, true, 0, ""
}
