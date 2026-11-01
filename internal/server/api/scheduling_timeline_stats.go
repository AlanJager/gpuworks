package api

import (
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The stats of the deployments that share GPUs, as the fleet's sharing stats
// and a node page's timeline answer them (docs/reference/api.md, section
// 驻留时间线). Neither reads the event log for them. The fleet's are each
// deployment's own wherever it ran (SchedulingActivity.Deployments); a node
// page's count only its instances on the named nodes: the awake and present
// time from the segments the page's lanes are derived from
// (store.SegmentCoverage), the yields and wakes from the activity's counts
// of those nodes (SchedulingActivity.DeploymentsOnNodes). The answer's scope
// says which.

// timelineWindow is a window a timeline answers for.
type timelineWindow struct {
	key  string
	span time.Duration
}

// option is the window as the answer lists it.
func (w timelineWindow) option() dto.TimelineWindowOption {
	return dto.TimelineWindowOption{Key: w.key, Seconds: int64(w.span / time.Second)}
}

// wire is the window ending at now as the answer names it.
func (w timelineWindow) wire(now time.Time) dto.TimelineWindow {
	return dto.TimelineWindow{From: now.Add(-w.span), To: now, Hours: int(w.span / time.Hour), Key: w.key, Seconds: int64(w.span / time.Second)}
}

// sharingFigures is what a row of the stats counts for a deployment, over
// the fleet or on some nodes.
type sharingFigures struct {
	store.ResidencySwitches
	awakeMS, presentMS int64
}

// fleetFigures is a deployment's figures across the fleet, from the
// activity: ok is false when its instances held no card in the window.
func fleetFigures(acts map[uint]store.DeploymentActivity, from, now time.Time) func(uint, store.ResidencyDeployment) (sharingFigures, bool) {
	return func(id uint, info store.ResidencyDeployment) (sharingFigures, bool) {
		act := acts[id]
		if act.HeldFrom.IsZero() {
			return sharingFigures{}, false
		}
		start, end := store.Presence(info, act.HeldFrom, act.HeldTo, from, now)
		return sharingFigures{ResidencySwitches: act.ResidencySwitches, awakeMS: act.AwakeMS, presentMS: end.Sub(start).Milliseconds()}, true
	}
}

// sharingRows are the stats of the deployments among ids that share cards
// over [from, now] (store.ListedInSharing) and whose instances held cards
// then, with the figures figuresOf gives (ok false: none held cards).
func (a *apiHandlers) sharingRows(ids []uint, figuresOf func(uint, store.ResidencyDeployment) (sharingFigures, bool), from, now time.Time) ([]dto.TimelineDeployment, error) {
	out := []dto.TimelineDeployment{}
	infos, err := a.d.Store.ResidencyDeployments(ids)
	if err != nil {
		return nil, err
	}
	var listed []uint
	figures := map[uint]sharingFigures{}
	for _, id := range ids {
		info, ok := infos[id]
		if !ok {
			continue
		}
		fig, held := figuresOf(id, info)
		if held && store.ListedInSharing(info, fig.ResidencySwitches) {
			listed = append(listed, id)
			figures[id] = fig
		}
	}
	if len(listed) == 0 {
		return out, nil
	}
	insts, err := a.d.Store.LiveCardsOf(listed)
	if err != nil {
		return nil, err
	}
	cards, err := a.cardsOf(placementsOf(insts), listed)
	if err != nil {
		return nil, err
	}
	waits := a.holdWaits(listed, from, now)
	for _, id := range listed {
		info, fig, wait := infos[id], figures[id], waits[id]
		awake, present := roundedSeconds(fig.awakeMS), roundedSeconds(fig.presentMS)
		share := 0.0
		if present > 0 {
			share = min(float64(awake)/float64(present), 1)
		}
		out = append(out, dto.TimelineDeployment{
			DeploymentID: id, Deployment: info.Name, DeploymentDeleted: info.Deleted, YieldPolicy: info.Yield,
			AwakeSeconds: awake, AwakeShare: share, PresentSeconds: present,
			Switches: fig.Yielded + fig.Wakes, Yielded: fig.Yielded, YieldedTo: fig.YieldedTo, Wakes: fig.Wakes,
			HeldRequests: wait.held, WaitP50Ms: wait.p50, WaitP95Ms: wait.p95,
			Cards: nonNilCards(cards[id]),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Deployment != out[j].Deployment {
			return out[i].Deployment < out[j].Deployment
		}
		return out[i].DeploymentID < out[j].DeploymentID
	})
	return out, nil
}

// roundedSeconds is a length in ms as whole seconds, 0 when not positive.
func roundedSeconds(ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	return (ms + 500) / 1000
}
