package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// FleetOccupant is one deployment holding a card: awake with its claim, or
// asleep with its residue.
type FleetOccupant struct {
	DeploymentID uint
	Asleep       bool
}

// FleetCard is one card as the ledger charges it, without the detail of who
// holds it: the figures GPUAllocations gives and which deployments hold it.
// Switching says some instance on the card is between states - starting,
// going to sleep, waking, draining.
type FleetCard struct {
	Index         int
	Name          string
	MemoryBytes   int64
	AwakeBytes    int64
	SleepingBytes int64
	HeldOutside   bool
	// Unhealthy: the card cannot take work for a reason other than being
	// held by a process outside the platform (its health, its ownership).
	Unhealthy bool
	Switching bool
	Occupants []FleetOccupant
}

// FleetDeployment is what the overview names a deployment by.
type FleetDeployment struct {
	Name         string
	ServiceLevel string
}

// FleetLedger is every node's cards, by node ID, in the order the node lists
// them, and the deployments by ID.
type FleetLedger struct {
	Cards       map[uint][]FleetCard
	Deployments map[uint]FleetDeployment
}

// FleetCards derives the ledger of all the given nodes in one pass: the live
// instances, deployments and the models of the sleepers are each read once
// for the fleet, not once per node as GPUAllocations and GPUOccupants do. The
// figures are the same ones, because the same arithmetic charges them
// (allocateInstances, occupantsOf).
func FleetCards(st *store.Store, nodes []store.Node) (*FleetLedger, error) {
	instances, err := st.LiveInstances()
	if err != nil {
		return nil, err
	}
	deployments, err := st.LedgerDeployments()
	if err != nil {
		return nil, err
	}
	out := &FleetLedger{Cards: make(map[uint][]FleetCard, len(nodes)), Deployments: make(map[uint]FleetDeployment, len(deployments))}
	modelOf := make(map[uint]uint, len(deployments))
	for i := range deployments {
		d := &deployments[i]
		level := d.ServiceLevel
		if level == "" {
			level = store.ServiceLevelResident
		}
		out.Deployments[d.ID] = FleetDeployment{Name: d.Name, ServiceLevel: level}
		modelOf[d.ID] = d.ModelID
	}

	byNode := make(map[uint][]store.Instance, len(nodes))
	// A follower of a multi-node replica sleeps with its leader, which is
	// another row of the same deployment: those deployments' rows are kept
	// together to look the leader up.
	byDeployment := map[uint][]store.Instance{}
	for i := range instances {
		in := &instances[i]
		byNode[in.NodeID] = append(byNode[in.NodeID], *in)
		if in.DistGroup != "" {
			byDeployment[in.DeploymentID] = append(byDeployment[in.DeploymentID], *in)
		}
	}
	reported := func(in *store.Instance) int {
		if !in.Headless || in.DistGroup == "" {
			return in.SleepLevel
		}
		return ReportedSleepLevelAmong(byDeployment[in.DeploymentID], in)
	}
	sleeper := func(in *store.Instance) bool {
		return in.State == proto.InstRunning && InstanceSleepIntent(in) > 0 && reported(in) > 0
	}

	// Only sleepers are charged a residue, which is sized from their model.
	wanted := map[uint]bool{}
	for i := range instances {
		if in := &instances[i]; sleeper(in) {
			wanted[modelOf[in.DeploymentID]] = true
		}
	}
	ids := make([]uint, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	models, err := st.LedgerModels(ids)
	if err != nil {
		return nil, err
	}
	byModel := make(map[uint]*store.Model, len(models))
	for i := range models {
		byModel[models[i].ID] = &models[i]
	}
	residue := newResidues(st)
	residue.known = func(deploymentID uint) float64 { return modelParams(byModel[modelOf[deploymentID]]) }

	for i := range nodes {
		node := &nodes[i]
		rows := byNode[node.ID]
		allocations := allocateInstances(st, node, rows, ledgerView{}, residue, sleeper)
		occupants := occupantsOf(st, node, rows, residue, sleeper)
		cards := make([]FleetCard, 0, len(node.GPUs))
		for _, gpu := range node.OwnedGPUs() {
			if gpu.Index < 0 {
				continue // a card the vendor tool does not list holds nothing schedulable
			}
			alloc := allocations[gpu.Index]
			card := FleetCard{
				Index: gpu.Index, Name: gpu.Name, MemoryBytes: gpu.MemoryTotal,
				AwakeBytes: alloc.AllocatedBytes, SleepingBytes: alloc.SleepingBytes, HeldOutside: gpu.HeldOutsidePlatform(),
				Unhealthy: !gpu.CanTakeWork() && !gpu.HeldOutsidePlatform(),
			}
			for _, o := range occupants[gpu.Index] {
				card.Occupants = append(card.Occupants, FleetOccupant{DeploymentID: o.Instance.DeploymentID, Asleep: o.Asleep})
				if inFlux(&o.Instance, reported) {
					card.Switching = true
				}
			}
			cards = append(cards, card)
		}
		out.Cards[node.ID] = cards
	}
	return out, nil
}

// inFlux reports whether an instance is between states: not running, or
// running with a sleep order its engine has not yet carried out (or a wake
// it has not finished).
func inFlux(in *store.Instance, reported func(*store.Instance) int) bool {
	if in.State != proto.InstRunning {
		return true
	}
	return (InstanceSleepIntent(in) > 0) != (reported(in) > 0)
}
