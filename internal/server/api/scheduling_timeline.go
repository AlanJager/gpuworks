package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const (
	// timelineSegmentCap bounds the segments one GPU lists; a card that
	// changed hands more often than that keeps the newest.
	timelineSegmentCap = 200
	// timelineMaxNodes bounds the nodes one request names (a board page).
	timelineMaxNodes = 200
	// timelineDefaultHours is the window when none is asked for.
	timelineDefaultHours = 6
)

// timelineWindows are the windows (hours) the timeline answers for.
var timelineWindows = []int{6, 24}

// schedulingTimeline answers GET /scheduling/timeline: for the nodes named by
// repeated node parameters (the board's current page), each GPU's residency
// over the last hours (6 or 24) - which deployment's instance held it and in
// what state - and the stats of the deployments that share GPUs.
//
// With sharing=1 and no node it answers the stats of the deployments that
// share GPUs across the fleet and no lanes.
//
// It is a separate read from the board so the board, which refreshes every few
// seconds, stays one cheap read: the timeline is derived from the event log
// over a window, once per page of nodes and window, by index ranges only
// (store.ResidencyInputs). It only reads.
func (a *apiHandlers) schedulingTimeline(w http.ResponseWriter, r *http.Request) {
	invalid := func(field, reason string) {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, field+" "+reason, map[string]any{"field": field})
	}
	hours := timelineDefaultHours
	if raw := strings.TrimSpace(r.URL.Query().Get("hours")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || !containsInt(timelineWindows, n) {
			invalid("hours", "is 6 or 24")
			return
		}
		hours = n
	}
	var uuids []string
	seen := map[string]bool{}
	for _, raw := range r.URL.Query()["node"] {
		for _, uuid := range strings.Split(raw, ",") {
			if uuid = strings.TrimSpace(uuid); uuid != "" && !seen[uuid] {
				seen[uuid] = true
				uuids = append(uuids, uuid)
			}
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	if sharing := strings.TrimSpace(r.URL.Query().Get("sharing")); sharing != "" {
		if sharing != "1" || len(uuids) > 0 {
			invalid("sharing", "is 1, and is asked for without node")
			return
		}
		out, err := a.cachedSharingTimeline(now, hours)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(uuids) == 0 || len(uuids) > timelineMaxNodes {
		invalid("node", "names between 1 and "+strconv.Itoa(timelineMaxNodes)+" nodes")
		return
	}
	out, err := a.residencyTimeline(uuids, now, hours)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func containsInt(list []int, n int) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}

// timelineCard is one GPU being filled in.
type timelineCard struct {
	out  *dto.TimelineGPU
	segs []dto.TimelineSegment
}

func (a *apiHandlers) residencyTimeline(uuids []string, now time.Time, hours int) (dto.SchedulingTimeline, error) {
	from := now.Add(-time.Duration(hours) * time.Hour)
	out := dto.SchedulingTimeline{
		Window: dto.TimelineWindow{From: from, To: now, Hours: hours},
		Nodes:  []dto.TimelineNode{}, Deployments: []dto.TimelineDeployment{},
	}
	var nodeIDs []uint
	cards := map[uint]map[int]*timelineCard{}
	for _, uuid := range uuids {
		node, err := a.d.Store.NodeWithGPUs(uuid)
		if errors.Is(err, store.ErrNotFound) {
			continue // gone since the board was read
		}
		if err != nil {
			return out, err
		}
		item := dto.TimelineNode{UUID: node.UUID, Name: nodeDisplayName(node), GPUs: []dto.TimelineGPU{}}
		for _, gpu := range node.OwnedGPUs() {
			if gpu.Index >= 0 {
				item.GPUs = append(item.GPUs, dto.TimelineGPU{Index: gpu.Index, Segments: []dto.TimelineSegment{}})
			}
		}
		out.Nodes = append(out.Nodes, item)
		nodeIDs = append(nodeIDs, node.ID)
		cards[node.ID] = map[int]*timelineCard{}
		for i := range out.Nodes[len(out.Nodes)-1].GPUs {
			gpu := &out.Nodes[len(out.Nodes)-1].GPUs[i]
			cards[node.ID][gpu.Index] = &timelineCard{out: gpu}
		}
	}
	insts, events, err := a.d.Store.ResidencyInputs(nodeIDs, from)
	if err != nil {
		return out, err
	}
	return a.fillTimeline(out, cards, insts, events, from, now)
}

// fillTimeline derives the segments of the instances into the cards' lanes
// (none for the fleet's sharing stats) and the stats of the shared deployments.
func (a *apiHandlers) fillTimeline(out dto.SchedulingTimeline, cards map[uint]map[int]*timelineCard, insts []store.ResidencyInstance, events map[string][]store.ResidencyEvent, from, now time.Time) (dto.SchedulingTimeline, error) {
	var err error
	type derived struct {
		inst store.ResidencyInstance
		segs []store.ResidencySegment
	}
	var all []derived
	depIDs := map[uint]bool{}
	for _, inst := range insts {
		segs := store.DeriveResidency(inst, events[inst.EventsOf()], from, now)
		if len(segs) > 0 {
			all = append(all, derived{inst, segs})
			depIDs[inst.DeploymentID] = true
		}
	}
	ids := make([]uint, 0, len(depIDs))
	for id := range depIDs {
		ids = append(ids, id)
	}
	deployments, err := a.d.Store.ResidencyDeployments(ids)
	if err != nil {
		return out, err
	}
	awake := map[uint][]interval{}
	for _, d := range all {
		info := deployments[d.inst.DeploymentID]
		for _, seg := range d.segs {
			if seg.State == store.ResidencyAwake {
				awake[d.inst.DeploymentID] = append(awake[d.inst.DeploymentID], interval{seg.From, seg.To})
			}
			wire := dto.TimelineSegment{
				DeploymentID: seg.DeploymentID, Deployment: info.Name, Instance: seg.Instance, State: string(seg.State),
				Level: seg.Level, From: seg.From.UTC(), To: seg.To.UTC(),
			}
			for _, index := range d.inst.GPUs {
				if card := cards[d.inst.NodeID][index]; card != nil {
					card.segs = append(card.segs, wire)
				}
			}
		}
	}
	for _, perNode := range cards {
		for _, card := range perNode {
			sort.SliceStable(card.segs, func(i, j int) bool {
				if !card.segs[i].From.Equal(card.segs[j].From) {
					return card.segs[i].From.Before(card.segs[j].From)
				}
				return card.segs[i].Instance < card.segs[j].Instance
			})
			if len(card.segs) > timelineSegmentCap {
				card.segs = card.segs[len(card.segs)-timelineSegmentCap:]
				card.out.Truncated = true
			}
			if card.segs != nil {
				card.out.Segments = card.segs
			}
		}
	}
	out.Deployments, err = a.timelineDeployments(deployments, awake, from, now)
	return out, err
}

// interval is a stretch of time.
type interval struct{ from, to time.Time }

// unionSeconds is the time the intervals cover together.
func unionSeconds(spans []interval) int64 {
	sort.Slice(spans, func(i, j int) bool { return spans[i].from.Before(spans[j].from) })
	var total time.Duration
	var end time.Time
	for _, span := range spans {
		start := span.from
		if start.Before(end) {
			start = end
		}
		if span.to.After(start) {
			total += span.to.Sub(start)
			end = span.to
		}
	}
	return int64(total.Seconds() + 0.5)
}

// timelineDeployments are the stats of the deployments that share GPUs.
func (a *apiHandlers) timelineDeployments(deployments map[uint]store.ResidencyDeployment, awake map[uint][]interval, from, now time.Time) ([]dto.TimelineDeployment, error) {
	var shared, every []uint
	for id, info := range deployments {
		every = append(every, id)
		if info.Yield != servicelevel.Never {
			shared = append(shared, id)
		}
	}
	out := []dto.TimelineDeployment{}
	if len(shared) == 0 {
		return out, nil
	}
	var counts map[uint]store.ResidencySwitches
	first, ok, err := a.d.Store.ResidencyWindowStart(from)
	if err != nil {
		return nil, err
	}
	if ok {
		if counts, err = a.d.Store.ResidencySwitchCounts(every, first); err != nil {
			return nil, err
		}
	}
	waits := a.holdWaits(shared, from, now)
	window := now.Sub(from).Seconds()
	for _, id := range shared {
		info, c, wait := deployments[id], counts[id], waits[id]
		seconds := unionSeconds(awake[id])
		out = append(out, dto.TimelineDeployment{
			DeploymentID: id, Deployment: info.Name, Preset: info.Preset,
			AwakeSeconds: seconds, AwakeShare: min(float64(seconds)/window, 1),
			Switches: c.Yielded + c.Wakes, Yielded: c.Yielded, YieldedTo: c.YieldedTo, Wakes: c.Wakes,
			HeldRequests: wait.held, WaitP50Ms: wait.p50, WaitP95Ms: wait.p95,
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
