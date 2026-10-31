package api

import (
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The stats of the deployments that share GPUs, as the fleet's sharing stats
// and a node page's timeline both answer them (docs/reference/api.md,
// section 驻留时间线). Both read the scheduling activity's rollups
// (SchedulingActivity.Deployments), the one count of a deployment's yields,
// wakes, awake and present time, so a deployment's figures are the same in
// either and neither reads the event log for them.

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

// sharingRows are the stats of the deployments among ids that share cards
// over [from, now] (store.ListedInSharing) and whose instances held cards
// then, with their figures from the activity.
func (a *apiHandlers) sharingRows(ids []uint, acts map[uint]store.DeploymentActivity, from, now time.Time) ([]dto.TimelineDeployment, error) {
	out := []dto.TimelineDeployment{}
	infos, err := a.d.Store.ResidencyDeployments(ids)
	if err != nil {
		return nil, err
	}
	var listed []uint
	for _, id := range ids {
		info, ok := infos[id]
		act := acts[id]
		if ok && !act.HeldFrom.IsZero() && store.ListedInSharing(info, act.ResidencySwitches) {
			listed = append(listed, id)
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
		info, act, wait := infos[id], acts[id], waits[id]
		awake := roundedSeconds(act.AwakeMS)
		start, end := store.Presence(info, act.HeldFrom, act.HeldTo, from, now)
		present := roundedSeconds(end.Sub(start).Milliseconds())
		share := 0.0
		if present > 0 {
			share = min(float64(awake)/float64(present), 1)
		}
		out = append(out, dto.TimelineDeployment{
			DeploymentID: id, Deployment: info.Name, DeploymentDeleted: info.Deleted, YieldPolicy: info.Yield,
			AwakeSeconds: awake, AwakeShare: share, PresentSeconds: present,
			Switches: act.Yielded + act.Wakes, Yielded: act.Yielded, YieldedTo: act.YieldedTo, Wakes: act.Wakes,
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
