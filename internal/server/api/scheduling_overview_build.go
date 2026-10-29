package api

import (
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// overviewYieldWindow is how far back the overview counts yields.
const overviewYieldWindow = time.Hour

// buildOverview derives the overview from the fleet in one pass: the nodes,
// the ledger of every node (planning.FleetCards) and the fleet's contention,
// each read once. Nothing here is per node.
func (a *apiHandlers) buildOverview(now time.Time) (*dto.SchedulingOverview, error) {
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		if left, right := nodes[i].DisplayName(), nodes[j].DisplayName(); left != right {
			return left < right
		}
		return nodes[i].UUID < nodes[j].UUID
	})
	ledger, err := planning.FleetCards(a.d.Store, nodes)
	if err != nil {
		return nil, err
	}
	pressure, err := a.overviewPressure(now)
	if err != nil {
		return nil, err
	}
	online := a.onlineOf(nodes)

	out := &dto.SchedulingOverview{Models: []string{}, Deployments: []dto.OverviewDeployment{}, Nodes: make([]dto.OverviewNode, 0, len(nodes))}
	modelAt := map[string]int{}
	deploymentAt := map[uint]int{}
	byModel := map[string]*dto.OverviewModelSummary{}
	summary := &out.Summary
	summary.Nodes = len(nodes)
	for i := range nodes {
		node := &nodes[i]
		isOnline := online(node.UUID)
		if isOnline {
			summary.OnlineNodes++
		}
		item := dto.OverviewNode{UUID: node.UUID, Name: nodeDisplayName(node), Online: isOnline, Cards: []dto.OverviewCard{}}
		if labels, err := store.NodeLabels(node.Labels); err == nil && len(labels) > 0 {
			item.Labels = labels
		}
		// A deployment's pressure lands on the nodes where it has a card: the
		// requests held for it on those where it sleeps, its yields on all.
		onNode := map[uint]bool{}
		asleepOnNode := map[uint]bool{}
		for _, card := range ledger.Cards[node.ID] {
			model, ok := modelAt[card.Name]
			if !ok {
				model = len(out.Models)
				modelAt[card.Name] = model
				out.Models = append(out.Models, card.Name)
				byModel[card.Name] = &dto.OverviewModelSummary{Model: card.Name}
			}
			wire := dto.OverviewCard{
				Index: card.Index, Model: model, MemoryMiB: card.MemoryBytes >> 20,
				AwakePct: percent(card.AwakeBytes, card.MemoryBytes),
			}
			wire.SleepingPct = min(percent(card.SleepingBytes, card.MemoryBytes), 100-wire.AwakePct)
			if card.HeldOutside {
				wire.Flags |= dto.OverviewHeldOutside
			}
			if card.Switching {
				wire.Flags |= dto.OverviewSwitching
			}
			if card.Unhealthy {
				wire.Flags |= dto.OverviewUnhealthy
			}
			for _, occupant := range card.Occupants {
				at, ok := deploymentAt[occupant.DeploymentID]
				if !ok {
					at = len(out.Deployments)
					deploymentAt[occupant.DeploymentID] = at
					info := ledger.Deployments[occupant.DeploymentID]
					out.Deployments = append(out.Deployments, dto.OverviewDeployment{ID: occupant.DeploymentID, Name: info.Name, Preset: info.Preset})
				}
				code := at * 2
				if occupant.Asleep {
					code++
					asleepOnNode[occupant.DeploymentID] = true
				}
				onNode[occupant.DeploymentID] = true
				wire.Occupants = append(wire.Occupants, code)
			}
			item.Cards = append(item.Cards, wire)
			count(summary, byModel[card.Name], isOnline, card, wire)
		}
		item.Contention = pressure.waitingOn[node.ID]
		for id := range asleepOnNode {
			item.Contention += pressure.held[id]
		}
		for id := range onNode {
			item.Contention += pressure.yields[id]
		}
		out.Nodes = append(out.Nodes, item)
	}
	out.Summary.Contention = dto.OverviewContention{WaitingWakes: len(pressure.waiting), HeldRequests: pressure.heldTotal, YieldsLastHour: pressure.yieldTotal}
	out.Summary.ByModel = make([]dto.OverviewModelSummary, 0, len(byModel))
	for _, model := range out.Models {
		out.Summary.ByModel = append(out.Summary.ByModel, *byModel[model])
	}
	sort.SliceStable(out.Summary.ByModel, func(i, j int) bool { return out.Summary.ByModel[i].Cards > out.Summary.ByModel[j].Cards })
	return out, nil
}

// percent is the share of total that bytes is, rounded to the nearest whole
// percent but never 0 for something held and never above 100.
func percent(bytes, total int64) int {
	if bytes <= 0 {
		return 0
	}
	if total <= 0 {
		return 100
	}
	return min(max(int((bytes*100+total/2)/total), 1), 100)
}

// count adds one card to the fleet's and its model's summary, in the first
// bucket that holds (dto.OverviewSummary).
func count(summary *dto.OverviewSummary, model *dto.OverviewModelSummary, online bool, card planning.FleetCard, wire dto.OverviewCard) {
	summary.Cards++
	model.Cards++
	switch {
	case !online:
		summary.Offline++
		model.Offline++
	case card.Unhealthy:
		summary.Unhealthy++
		model.Unhealthy++
	case card.HeldOutside:
		summary.HeldOutside++
		model.HeldOutside++
	case card.AwakeBytes > 0:
		summary.Awake++
		model.Awake++
	case card.SleepingBytes > 0:
		summary.Asleep++
		model.Asleep++
	default:
		summary.Free++
		model.Free++
	}
}

// overviewPressure is what the deployments push on each other for cards,
// read once for the fleet.
type overviewPressure struct {
	// waiting are the deployments with a sleeper whose wake waits for room;
	// waitingOn counts those sleepers by node.
	waiting   map[uint]bool
	waitingOn map[uint]int
	// held are the requests the gateway holds for each deployment's wake.
	held      map[uint]int
	heldTotal int
	// yields are the yields of the last hour by deployment.
	yields     map[uint]int
	yieldTotal int
}

func (a *apiHandlers) overviewPressure(now time.Time) (overviewPressure, error) {
	p := overviewPressure{waiting: map[uint]bool{}, waitingOn: map[uint]int{}, held: map[uint]int{}, yields: map[uint]int{}}
	waiting, err := a.d.Store.InstancesWakeWaiting()
	if err != nil {
		return p, err
	}
	for _, in := range waiting {
		p.waiting[in.DeploymentID] = true
		p.waitingOn[in.NodeID]++
	}
	if a.d.WakeHeld != nil {
		for id, n := range a.d.WakeHeld() {
			if n > 0 {
				p.held[id] = n
				p.heldTotal += n
			}
		}
	}
	events, err := a.d.Store.YieldEvents(now.Add(-overviewYieldWindow))
	if err != nil {
		return p, err
	}
	for _, ev := range events {
		if store.IsYield(ev) {
			p.yields[ev.DeploymentID]++
			p.yieldTotal++
		}
	}
	return p, nil
}
