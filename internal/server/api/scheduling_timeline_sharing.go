package api

import (
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// sharingTTL is how long the fleet's sharing stats are kept: they are read
// from the event log over a window of hours for every shared deployment, so a
// page polling them and a few viewers share one read.
const sharingTTL = 30 * time.Second

// sharingCache keeps the last sharing stats of each window.
type sharingCache struct {
	mu      sync.Mutex
	byHours map[int]sharingEntry
	reads   atomic.Int64
}

type sharingEntry struct {
	out dto.SchedulingTimeline
	at  time.Time
}

func (a *apiHandlers) cachedSharingTimeline(now time.Time, hours int) (dto.SchedulingTimeline, error) {
	c := &a.overview.sharing
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.byHours[hours]; ok && time.Since(entry.at) < sharingTTL {
		return entry.out, nil
	}
	out, err := a.sharingTimeline(now, hours)
	if err != nil {
		return out, err
	}
	if c.byHours == nil {
		c.byHours = map[int]sharingEntry{}
	}
	c.byHours[hours] = sharingEntry{out: out, at: time.Now()}
	c.reads.Add(1)
	return out, nil
}

// sharingTimeline is the stats of the deployments that share GPUs across the
// fleet, with no node's lanes: what the scheduling center shows beside the
// overview, read for the shared deployments' instances only.
func (a *apiHandlers) sharingTimeline(now time.Time, hours int) (dto.SchedulingTimeline, error) {
	from := now.Add(-time.Duration(hours) * time.Hour)
	out := dto.SchedulingTimeline{
		Window: dto.TimelineWindow{From: from, To: now, Hours: hours},
		Nodes:  []dto.TimelineNode{}, Deployments: []dto.TimelineDeployment{},
	}
	ids, err := a.d.Store.SharedDeploymentIDs()
	if err != nil {
		return out, err
	}
	insts, events, err := a.d.Store.ResidencyInputsOf(ids, from)
	if err != nil {
		return out, err
	}
	return a.fillTimeline(out, nil, insts, events, from, now)
}
