package deployment

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// imageTransferNodes resolves only the distinct references needed by this
// read, using the same catalog identity/alias precedence as image delivery.
// The ledger is batched; a fleet without active transfers needs no catalog
// reads. The rows already carry current terminal states, rather than an old
// scheduling event being treated as evidence that a copy is still alive.
func imageTransferNodes(st *store.Store, deployments []store.Deployment) (map[string][]uint, error) {
	if len(deployments) == 0 {
		return nil, nil
	}
	active, err := st.ActiveImageTransfers()
	if err != nil {
		return nil, err
	}
	out := map[string][]uint{}
	if len(active) == 0 {
		return out, nil
	}
	byImage := map[uint][]uint{}
	for _, row := range active {
		byImage[row.ImageVersionID] = append(byImage[row.ImageVersionID], row.NodeID)
	}
	for _, dep := range deployments {
		if dep.Image == "" {
			continue
		}
		if _, seen := out[dep.Image]; seen {
			continue
		}
		image, err := st.ResolveImageRef(dep.Image)
		if err == store.ErrNotFound {
			out[dep.Image] = nil
			continue
		}
		if err != nil {
			return nil, err
		}
		out[dep.Image] = byImage[image.ID]
	}
	return out, nil
}

func (l StoreRows) ImageTransferNodes(image string) []uint {
	if l.imageTransfers != nil {
		return l.imageTransfers[image]
	}
	nodes, err := imageTransferNodes(l.st, []store.Deployment{{Image: image}})
	if err != nil {
		return nil
	}
	return nodes[image]
}

func (l *ListRows) ImageTransferNodes(image string) []uint { return l.imageTransfers[image] }

func waitingForImage(ev store.InstanceEvent) bool {
	var data struct {
		Scheduler *scheduler.Reason `json:"scheduler"`
	}
	return json.Unmarshal([]byte(ev.Data), &data) == nil && data.Scheduler != nil && data.Scheduler.Code == scheduler.ReasonImageDeliveryWaiting
}

// Only deployments waiting for an image need this join. Running and genuinely
// unschedulable deployments do not pay a catalog lookup for an unrelated copy.
func (l *ListRows) loadImageTransfers() error {
	var preparing []store.Deployment
	for _, dep := range l.deploymentRows {
		if dep.State != store.DeployStopped && dep.State != store.DeployDeleting && len(l.instancesByDeployment[dep.ID]) == 0 && waitingForImage(l.latest[dep.ID]) {
			preparing = append(preparing, dep)
		}
	}
	var err error
	l.imageTransfers, err = imageTransferNodes(l.st, preparing)
	return err
}

// WithImageTransfers preloads the new fallible image facts for a detail HTTP
// read, so a failed ledger read is returned as an error rather than silently
// interpreting it as a finished transfer. Other rows remain lazy as before.
func (l StoreRows) WithImageTransfers(dep *store.Deployment, instanceCount int) (StoreRows, error) {
	l.imageTransfers = map[string][]uint{}
	if instanceCount > 0 || dep.State == store.DeployStopped || dep.State == store.DeployDeleting || dep.Image == "" {
		return l, nil
	}
	latest, found, err := l.st.LatestScheduleEvent(dep.ID)
	if err != nil {
		return l, err
	}
	if !found || !waitingForImage(latest) {
		return l, nil
	}
	l.imageTransfers, err = imageTransferNodes(l.st, []store.Deployment{*dep})
	return l, err
}
