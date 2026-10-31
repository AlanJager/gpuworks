package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const (
	// timelineSegmentCap bounds the segments one GPU lists; a card that
	// changed hands more often than that keeps the newest.
	timelineSegmentCap = 200
	// timelineMaxNodes bounds the nodes one request names (a board page).
	timelineMaxNodes = 200
)

// nodeWindows are the windows of a node's timeline (its lanes), in the order
// the UI offers them; the first is the one when none is asked for.
var nodeWindows = []timelineWindow{{"6h", 6 * time.Hour}, {"24h", 24 * time.Hour}}

// timelineHours are the windows the older hours parameter names (6 or 24).
var timelineHours = []int{6, 24}

// schedulingTimeline answers GET /scheduling/timeline: for the nodes named by
// repeated node parameters (the board's current page), each GPU's residency
// over a window (window, one of nodeWindows) - which deployment's instance
// held it and in what state - and the stats of the deployments that share
// GPUs.
//
// With sharing=1 and no node it answers the stats of the deployments that
// share GPUs across the fleet over one of sharingWindows and no lanes.
// hours=6|24 still names the 6h or 24h window.
//
// It is a separate read from the board so the board, which refreshes every few
// seconds, stays one cheap read: the lanes are derived from the event log
// over a window, once per page of nodes and window, by index ranges only
// (store.ResidencyInputs); the stats come from the scheduling activity's
// rollups. It only reads.
func (a *apiHandlers) schedulingTimeline(w http.ResponseWriter, r *http.Request) {
	invalid := func(field, reason string) {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, field+" "+reason, map[string]any{"field": field})
	}
	if a.d.SchedulingActivity == nil {
		writeErr(w, http.StatusNotFound, ErrSchedulingActivityUnavailable)
		return
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
	sharing := strings.TrimSpace(r.URL.Query().Get("sharing"))
	windows, win := nodeWindows, nodeWindows[0]
	if sharing != "" {
		windows, win = sharingWindows, sharingDefault
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("hours")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || !containsInt(timelineHours, n) {
			invalid("hours", "is one of "+joinInts(timelineHours))
			return
		}
		win, _ = windowOf(windows, strconv.Itoa(n)+"h")
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("window")); raw != "" {
		var ok bool
		if win, ok = windowOf(windows, raw); !ok {
			invalid("window", "is one of "+windowKeys(windows))
			return
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	if sharing != "" {
		if sharing != "1" || len(uuids) > 0 {
			invalid("sharing", "is 1, and is asked for without node")
			return
		}
		out, err := a.cachedSharingTimeline(now, win)
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
	out, err := a.residencyTimeline(uuids, now, win)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func windowOf(windows []timelineWindow, key string) (timelineWindow, bool) {
	for _, w := range windows {
		if w.key == key {
			return w, true
		}
	}
	return timelineWindow{}, false
}

func windowKeys(windows []timelineWindow) string {
	keys := make([]string, len(windows))
	for i, w := range windows {
		keys[i] = w.key
	}
	return strings.Join(keys, ", ")
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

func (a *apiHandlers) residencyTimeline(uuids []string, now time.Time, win timelineWindow) (dto.SchedulingTimeline, error) {
	out := dto.SchedulingTimeline{
		Scope: dto.TimelineScopeNode, Window: win.wire(now), Windows: timelineOptions(nodeWindows),
		Nodes: []dto.TimelineNode{}, Deployments: []dto.TimelineDeployment{},
	}
	from := out.Window.From
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
	nodeUUIDs := make([]string, len(out.Nodes))
	for i, node := range out.Nodes {
		nodeUUIDs[i] = node.UUID
	}
	return a.fillTimeline(out, nodeUUIDs, cards, insts, events, from, now)
}

// fillTimeline derives the segments of the instances into the cards' lanes
// and lists the stats of the deployments whose instances held them, counting
// only those instances: their awake and present time from the same segments,
// their yields and wakes from the scheduling activity's counts of the nodes.
func (a *apiHandlers) fillTimeline(out dto.SchedulingTimeline, nodeUUIDs []string, cards map[uint]map[int]*timelineCard, insts []store.ResidencyInstance, events map[string][]store.ResidencyEvent, from, now time.Time) (dto.SchedulingTimeline, error) {
	type derived struct {
		inst store.ResidencyInstance
		segs []store.ResidencySegment
	}
	var all []derived
	depIDs := map[uint]bool{}
	segsOf := map[uint][]store.ResidencySegment{}
	for _, inst := range insts {
		segs := store.DeriveResidency(inst, events[inst.EventsOf()], from, now)
		if len(segs) > 0 {
			all = append(all, derived{inst, segs})
			depIDs[inst.DeploymentID] = true
			segsOf[inst.DeploymentID] = append(segsOf[inst.DeploymentID], segs...)
		}
	}
	ids := make([]uint, 0, len(depIDs))
	for id := range depIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	names, err := a.d.Store.ResidencyDeployments(ids)
	if err != nil {
		return out, err
	}
	for _, d := range all {
		name := names[d.inst.DeploymentID].Name
		for _, seg := range d.segs {
			wire := dto.TimelineSegment{
				DeploymentID: seg.DeploymentID, Deployment: name, Instance: seg.Instance, State: string(seg.State),
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
	switches := a.d.SchedulingActivity.DeploymentsOnNodes(nodeUUIDs, from, now, depIDs)
	out.Deployments, err = a.sharingRows(ids, func(id uint, _ store.ResidencyDeployment) (sharingFigures, bool) {
		awake, present := store.SegmentCoverage(segsOf[id])
		return sharingFigures{ResidencySwitches: switches[id], awakeMS: awake, presentMS: present}, present > 0
	}, from, now)
	return out, err
}

// joinInts lists numbers for a message: "6, 24".
func joinInts(values []int) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}
