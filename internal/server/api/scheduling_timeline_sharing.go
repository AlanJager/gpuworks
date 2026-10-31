package api

import (
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// sharingWindows are the windows of the fleet's sharing stats, in the order
// the UI offers them; sharingDefault is the one when none is asked for.
var (
	sharingWindows = []timelineWindow{
		{"15m", 15 * time.Minute}, {"1h", time.Hour}, {"6h", 6 * time.Hour}, {"24h", 24 * time.Hour},
	}
	sharingDefault = sharingWindows[2]
)

// sharingTTL is how long the fleet's sharing stats are kept: a page polling
// them and a few viewers share one read of the deployments and the
// gateway's held requests.
const sharingTTL = 30 * time.Second

// sharingCache keeps the last sharing stats of each window.
type sharingCache struct {
	mu    sync.Mutex
	byKey map[string]sharingEntry
	reads atomic.Int64
}

type sharingEntry struct {
	out dto.SchedulingTimeline
	at  time.Time
}

func (a *apiHandlers) cachedSharingTimeline(now time.Time, win timelineWindow) (dto.SchedulingTimeline, error) {
	c := &a.overview.sharing
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.byKey[win.key]; ok && time.Since(entry.at) < sharingTTL {
		return entry.out, nil
	}
	out, err := a.sharingTimeline(now, win)
	if err != nil {
		return out, err
	}
	if c.byKey == nil {
		c.byKey = map[string]sharingEntry{}
	}
	c.byKey[win.key] = sharingEntry{out: out, at: time.Now().UTC()}
	c.reads.Add(1)
	return out, nil
}

// sharingTimeline is the stats of the deployments that share GPUs across the
// fleet, with no node's lanes: what the scheduling center shows beside the
// overview. The candidates are the deployments that share cards now
// (store.SharedDeploymentIDs) and those that took part in a yield over the
// window, deleted ones included; their figures come from the scheduling
// activity, so nothing reads the event log.
func (a *apiHandlers) sharingTimeline(now time.Time, win timelineWindow) (dto.SchedulingTimeline, error) {
	out := dto.SchedulingTimeline{
		Window: win.wire(now), Windows: timelineOptions(sharingWindows),
		Nodes: []dto.TimelineNode{}, Deployments: []dto.TimelineDeployment{},
	}
	from := out.Window.From
	acts := a.d.SchedulingActivity.Deployments(from, now, nil)
	shared, err := a.d.Store.SharedDeploymentIDs()
	if err != nil {
		return out, err
	}
	seen := map[uint]bool{}
	ids := make([]uint, 0, len(shared))
	for _, id := range shared {
		seen[id] = true
		ids = append(ids, id)
	}
	for id, act := range acts {
		if !seen[id] && act.TookPartInYields() {
			ids = append(ids, id)
		}
	}
	out.Deployments, err = a.sharingRows(ids, acts, from, now)
	return out, err
}

func timelineOptions(windows []timelineWindow) []dto.TimelineWindowOption {
	out := make([]dto.TimelineWindowOption, len(windows))
	for i, w := range windows {
		out[i] = w.option()
	}
	return out
}
