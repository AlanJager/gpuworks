package app

import (
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// batchModels tells the batch service which served names the platform has and
// what each is to a batch. The create check (Lookup) and the list a client
// chooses from (List) both judge a name by nameState, so they cannot disagree.
type batchModels struct {
	st *store.Store
}

func (m batchModels) Lookup(name string) (batch.ModelState, error) {
	rows, err := m.st.DeploymentsByServedName(name)
	if err != nil {
		return batch.ModelUnknown, err
	}
	instances := map[uint][]store.Instance{}
	for _, d := range rows {
		insts, err := m.st.InstancesForDeployment(d.ID)
		if err != nil {
			return batch.ModelUnknown, err
		}
		instances[d.ID] = insts
	}
	return nameState(rows, instances), nil
}

// List is every served name of an active deployment with its state: the
// deployments and the live instances are each read once.
func (m batchModels) List() ([]batch.ModelEntry, error) {
	deployments, err := m.st.ListDeployments()
	if err != nil {
		return nil, err
	}
	live, err := m.st.LiveInstances()
	if err != nil {
		return nil, err
	}
	instances := map[uint][]store.Instance{}
	for _, in := range live {
		instances[in.DeploymentID] = append(instances[in.DeploymentID], in)
	}
	byName := map[string][]store.Deployment{}
	for _, d := range deployments {
		if d.State == store.DeployActive && d.ServedName != "" {
			byName[d.ServedName] = append(byName[d.ServedName], d)
		}
	}
	out := make([]batch.ModelEntry, 0, len(byName))
	for name, rows := range byName {
		out = append(out, batch.ModelEntry{Name: name, State: nameState(rows, instances)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State.Better(out[j].State)
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// nameState is what a served name is to a batch: the best of its active
// deployments (rows), none being unknown.
func nameState(rows []store.Deployment, instances map[uint][]store.Instance) batch.ModelState {
	best := batch.ModelUnknown
	for i := range rows {
		if state := deploymentState(&rows[i], instances[rows[i].ID]); state.Better(best) {
			best = state
		}
	}
	return best
}

// deploymentState is what one active deployment is to a batch: awake (or
// starting) it serves; asleep, a request wakes it when
// Deployment.WakesOnRequest says so - the gateway's rule - and otherwise the
// sleep is held. A replica awake with no sleep ordered serves, held or not:
// an operator's sleep of some replicas holds the deployment's sleep
// (Deployment.SleepHeld) while the others serve. Otherwise a held sleep is
// asleep whatever the instances report yet, and a deployment not held is
// asleep when it has running replicas and the gateway would refuse each for
// sleep (planning.InstanceAsleep on their leaders).
func deploymentState(d *store.Deployment, insts []store.Instance) batch.ModelState {
	if anyReplicaServes(insts) || !d.SleepHeld && !replicasAsleep(insts) {
		return batch.ModelAvailable
	}
	if d.WakesOnRequest() {
		return batch.ModelAsleep
	}
	return batch.ModelSleepHeld
}

// anyReplicaServes says a running replica takes requests: its leader is not
// asleep and is not ordered to sleep.
func anyReplicaServes(insts []store.Instance) bool {
	for i := range insts {
		in := &insts[i]
		if in.State == proto.InstRunning && !in.Headless && !planning.InstanceAsleep(in) {
			return true
		}
	}
	return false
}

// replicasAsleep says the deployment has running replicas and every one is
// asleep. A follower of a multi-node replica sleeps with its leader and is not
// judged on its own.
func replicasAsleep(insts []store.Instance) bool {
	running := 0
	for i := range insts {
		in := &insts[i]
		if in.State != proto.InstRunning || in.Headless {
			continue
		}
		running++
		if !planning.InstanceAsleep(in) {
			return false
		}
	}
	return running > 0
}
