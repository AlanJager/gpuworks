package store

import (
	"encoding/json"
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// The scheduling activity (docs/reference/api.md, section 调度活动) counts the
// scheduling events as they are written, by reason, by card and by
// deployment, so that no page reads the event log over a window. This file is
// the one place that says which reason an event counts under and which cards
// it concerns; the counting itself is internal/server/schedactivity.

// ActivityReason is why the scheduling activity counts an event.
type ActivityReason string

const (
	// ActivityIdleYield: the residency arbiter put an idle holder to sleep
	// for a waiter (AUTO_SLEEP, trigger preempt, reason idle).
	ActivityIdleYield ActivityReason = "idle_yield"
	// ActivityRankPreemption: a waiter of a higher rank took a preemptible
	// holder's cards (reason rank, or a preemption that names no reason).
	ActivityRankPreemption ActivityReason = "rank_preemption"
	// ActivityTimesliceTurn: equals took turns and the holder had served its
	// time (reason turn).
	ActivityTimesliceTurn ActivityReason = "timeslice_turn"
	// ActivityOperatorYield: an operator's yielding wake put the holder to
	// sleep (YIELDED_FOR_WAKE).
	ActivityOperatorYield ActivityReason = "operator_yield"
	// ActivityScheduled: a schedule slept, woke or scaled a deployment.
	ActivityScheduled ActivityReason = "scheduled"
	// ActivityPlacement: the platform placed a new replica on a node
	// (UNIT_PLACED) or moved one off a node an operator drains
	// (NODE_DRAIN_MOVED). A replica moved off a lost node is a failure.
	ActivityPlacement ActivityReason = "placement"
	// ActivityFailure: a replica rebuilt, a node replaced, a unit out of
	// retries, a rolling update stalled, a wake that timed out waiting, an
	// automatic order that failed.
	ActivityFailure ActivityReason = "failure"
	// ActivityQueued: a deployment could not be placed, or a wake waits for
	// room.
	ActivityQueued ActivityReason = "queued"
)

// activityReasons is the order the reasons are listed and stacked in.
var activityReasons = []ActivityReason{
	ActivityIdleYield, ActivityRankPreemption, ActivityTimesliceTurn, ActivityOperatorYield,
	ActivityScheduled, ActivityPlacement, ActivityFailure, ActivityQueued,
}

// ActivityReasons lists every reason, in the order they are shown.
func ActivityReasons() []ActivityReason { return append([]ActivityReason(nil), activityReasons...) }

// ActivityReasonIndex is the reason's position in ActivityReasons, -1 for
// none.
func ActivityReasonIndex(r ActivityReason) int {
	for i, have := range activityReasons {
		if have == r {
			return i
		}
	}
	return -1
}

// The codes of a replica placed or moved by a drain, as the controller writes
// them (pinned by scheduling_catalogue_test.go in the app package).
const (
	codeUnitPlaced     = "UNIT_PLACED"
	codeNodeDrainMoved = "NODE_DRAIN_MOVED"
)

// triggerSchedule is the trigger of a schedule's orders, as the automation
// writes it (pinned by scheduling_catalogue_test.go in the app package).
const triggerSchedule = "schedule"

// SchedulingActivityReason is the reason the scheduling activity counts an
// event under; ok is false for an event it does not count: a wake a request,
// a resume or a freed card brought (the other half of a yield, counted as
// the card's switch but not as a reason of its own) and an operator's order.
func SchedulingActivityReason(code, data string) (ActivityReason, bool) {
	var params struct {
		Trigger string `json:"trigger"`
		Reason  string `json:"reason"`
	}
	if code == codeAutoSleep || code == codeAutoWake {
		_ = json.Unmarshal([]byte(data), &params)
	}
	switch code {
	case codeAutoSleep:
		switch params.Trigger {
		case triggerPreempt:
			switch yieldpolicy.Reason(params.Reason) {
			case yieldpolicy.ReasonIdle:
				return ActivityIdleYield, true
			case yieldpolicy.ReasonTurn:
				return ActivityTimesliceTurn, true
			}
			return ActivityRankPreemption, true
		case triggerSchedule:
			return ActivityScheduled, true
		}
		return "", false
	case codeAutoWake:
		if params.Trigger == triggerSchedule {
			return ActivityScheduled, true
		}
		return "", false
	case "AUTO_SCALE":
		return ActivityScheduled, true
	case codeYieldedForWake:
		return ActivityOperatorYield, true
	case codeUnitPlaced, codeNodeDrainMoved:
		return ActivityPlacement, true
	case "WAKE_WAITING", EventDeploymentUnschedulable:
		return ActivityQueued, true
	case "WAKE_WAIT_TIMEOUT", "SCHEDULE_ACTION_FAILED":
		return ActivityFailure, true
	}
	if category, ok := SchedulingCategoryOf(code); ok && category == SchedulingFailure {
		return ActivityFailure, true
	}
	return "", false
}

// EventCard is one GPU an event concerns: its node's UUID and its index.
type EventCard struct {
	NodeUUID string
	GPU      int
}

// instancePlacement is where an instance holds cards: its node's UUID and
// the card indexes.
type instancePlacement struct {
	nodeUUID string
	gpus     []int
}

// EventCards is the cards an event concerns: those of the instances it names
// (EventInstances), as placed says they are held. It is the one definition
// of an event's cards; an instance placed says nothing about adds none. Each
// card is listed once, sorted.
func eventCards(instances []string, placed map[string]instancePlacement) []EventCard {
	var out []EventCard
	seen := map[EventCard]bool{}
	for _, uuid := range instances {
		p, ok := placed[uuid]
		if !ok || p.nodeUUID == "" {
			continue
		}
		for _, gpu := range p.gpus {
			card := EventCard{NodeUUID: p.nodeUUID, GPU: gpu}
			if !seen[card] {
				seen[card] = true
				out = append(out, card)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeUUID != out[j].NodeUUID {
			return out[i].NodeUUID < out[j].NodeUUID
		}
		return out[i].GPU < out[j].GPU
	})
	return out
}

// instancePlacements reads where the named live instances are: one indexed
// read of the instances and their nodes. An instance not placed or no longer
// known is absent.
func (s *Store) instancePlacements(uuids []string) (map[string]instancePlacement, error) {
	out := map[string]instancePlacement{}
	if len(uuids) == 0 {
		return out, nil
	}
	type placementRow struct{ Instance, Node, Indexes string }
	var rows []placementRow
	for start := 0; start < len(uuids); start += residencyEventChunk {
		end := min(start+residencyEventChunk, len(uuids))
		var page []placementRow
		if err := s.reader().Table("instances").Select("instances.uuid AS instance, nodes.uuid AS node, instances.gp_uindexes AS indexes").
			Joins("JOIN nodes ON nodes.id = instances.node_id").Where("instances.uuid IN ?", uuids[start:end]).Scan(&page).Error; err != nil {
			return nil, err
		}
		rows = append(rows, page...)
	}
	for _, row := range rows {
		out[row.Instance] = instancePlacement{nodeUUID: row.Node, gpus: instanceGPUIndexes(&Instance{GPUIndexes: row.Indexes})}
	}
	return out, nil
}

// schedulingPlacement is what a scheduling history event is written with and
// the cards it concerns, read while the instances it names still exist: the
// nodes it concerns are added to its params (EventParamNodeUUIDs) when its
// writer named none, and the cards are the instances' (eventCards). An event
// of another code, or one that names no instance, is left as it is and
// concerns no card. The caller's map is not changed.
func (s *Store) schedulingPlacement(code, rowInstance string, params map[string]any) (map[string]any, []EventCard) {
	if _, ok := SchedulingEventKind(code); !ok {
		return params, nil
	}
	instances := EventInstances(rowInstance, params)
	if len(instances) == 0 {
		return params, nil
	}
	_, namedUUIDs := params[EventParamNodeUUIDs]
	_, namedNode := params["node"]
	if (namedUUIDs || namedNode) && !isSwitchCode(code) {
		return params, nil // its nodes are named and it changes no card's hands
	}
	placed, err := s.instancePlacements(instances)
	if err != nil {
		return params, nil
	}
	cards := eventCards(instances, placed)
	if namedUUIDs || namedNode {
		return params, cards
	}
	nodes := map[string]bool{}
	for _, p := range placed {
		nodes[p.nodeUUID] = true
	}
	if len(nodes) == 0 {
		return params, cards
	}
	list := make([]string, 0, len(nodes))
	for uuid := range nodes {
		list = append(list, uuid)
	}
	sort.Strings(list)
	out := make(map[string]any, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	out[EventParamNodeUUIDs] = list
	return out, cards
}

// ObservedEvent is an event the scheduling activity counts, as the store
// hands it over once written (SetEventObserver) and as the backfill reads it
// back (SchedulingActivitySince): one path for both.
type ObservedEvent struct {
	ID           uint
	At           time.Time
	DeploymentID uint
	InstanceUUID string
	// Reason and Counted: the reason the activity counts it under.
	Reason  ActivityReason
	Counted bool
	// Yield: a deployment gave its cards up (IsYield), for ForDeployment
	// when that names one; Wake: the platform woke it (IsAutoWake).
	Yield, Wake   bool
	ForDeployment uint
	// Cards are the cards it concerns (eventCards).
	Cards []EventCard
	// Residency is a state, sleep or removal event of an instance, decoded:
	// what says whether the instance is awake and holds cards.
	Residency *ResidencyEvent
}

// observedEvent turns a written event into what the activity counts; ok is
// false for an event it has no use for.
func observedEvent(row InstanceEvent, cards []EventCard) (ObservedEvent, bool) {
	ev := ObservedEvent{ID: row.ID, At: row.CreatedAt.UTC(), DeploymentID: row.DeploymentID, InstanceUUID: row.InstanceUUID}
	if isResidencyCode(row.Code) {
		decoded := decodeResidencyEvent(row)
		ev.Residency = &decoded
		return ev, true
	}
	if _, ok := SchedulingEventKind(row.Code); !ok {
		return ev, false
	}
	ev.Reason, ev.Counted = SchedulingActivityReason(row.Code, row.Data)
	ev.Yield, ev.Wake = IsYield(row), IsAutoWake(row)
	if ev.Yield {
		ev.ForDeployment = EventForDeployment(row.Data)
	}
	if ev.Yield || ev.Wake {
		ev.Cards = cards
	}
	return ev, ev.Counted || ev.Yield || ev.Wake
}

// isSwitchCode says an event of the code may change a card's hands (a yield
// or a wake: IsYield, IsAutoWake), so its cards are wanted.
func isSwitchCode(code string) bool {
	return code == codeAutoSleep || code == codeAutoWake || code == codeYieldedForWake
}

// InstanceUUIDs is the UUID of every instance row: what the scheduling
// activity checks the instances it follows against, so that one deleted
// without a removal event (it held no card) is not followed for ever.
func (s *Store) InstanceUUIDs() (map[string]bool, error) {
	var uuids []string
	if err := s.reader().Model(&Instance{}).Pluck("uuid", &uuids).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(uuids))
	for _, uuid := range uuids {
		out[uuid] = true
	}
	return out, nil
}

func isResidencyCode(code string) bool {
	for _, c := range residencyWindowCode {
		if c == code {
			return true
		}
	}
	return false
}

// EventObserver receives each event the scheduling activity counts once it
// is written. It runs on the writer's goroutine and must not block.
type EventObserver func(ObservedEvent)

// SetEventObserver hands every event the scheduling activity counts, once it
// is committed, to fn (nil stops it).
func (s *Store) SetEventObserver(fn EventObserver) {
	if fn == nil {
		s.observer.Store(nil)
		return
	}
	s.observer.Store(&fn)
}

// observe hands a committed event to the observer, if any.
func (s *Store) observe(row InstanceEvent, cards []EventCard) {
	fn := s.observer.Load()
	if fn == nil || row.ID == 0 {
		return
	}
	if ev, ok := observedEvent(row, cards); ok {
		(*fn)(ev)
	}
}

// ListedInSharing says a deployment has a row in the stats of the
// deployments that share cards: one that exists and yields at all (any
// policy but never), or a deleted one that gave its cards up or was given
// another's over the window (its policy is not kept, so what it did is what
// says it shared).
func ListedInSharing(info ResidencyDeployment, c ResidencySwitches) bool {
	if info.Deleted {
		return c.TookPartInYields()
	}
	return info.Yield != yieldpolicy.Never
}

// Presence is the part of [from, now] a deployment existed in: from its
// creation, or from when its instances first held cards (seenFrom) when that
// is earlier (a deleted deployment's creation is not kept), to its deletion,
// or to when its instances last held cards (seenTo) when that is not known,
// and to now while it exists. It is the one definition the sharing stats and
// the activity's hour rows use.
func Presence(info ResidencyDeployment, seenFrom, seenTo, from, now time.Time) (start, end time.Time) {
	start, end = seenFrom, now
	if !info.Since.IsZero() && info.Since.Before(start) {
		start = info.Since
	}
	if start.Before(from) {
		start = from
	}
	if info.Deleted {
		end = seenTo
		if !info.Until.IsZero() && info.Until.Before(now) {
			end = info.Until
		}
	}
	return start, end
}

// DeploymentActivity is a deployment's figures over a window, as the
// scheduling activity counted them.
type DeploymentActivity struct {
	ResidencySwitches
	// AwakeMS is how long at least one of its instances was awake.
	AwakeMS int64
	// HeldFrom and HeldTo span the time its instances held cards in the
	// window, in any state; zero when they held none.
	HeldFrom, HeldTo time.Time
}

// LiveCardsOf is where the instances of some deployments hold cards now: one
// read by the deployment index, with each instance's node and card indexes
// (Live set); an instance holding no card is left out.
func (s *Store) LiveCardsOf(deploymentIDs []uint) ([]ResidencyInstance, error) {
	if len(deploymentIDs) == 0 {
		return nil, nil
	}
	var rows []Instance
	if err := s.reader().Select("UUID", "DeploymentID", "NodeID", "GPUIndexes").Where("deployment_id IN ?", deploymentIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ResidencyInstance, 0, len(rows))
	for i := range rows {
		if gpus := instanceGPUIndexes(&rows[i]); len(gpus) > 0 {
			out = append(out, ResidencyInstance{UUID: rows[i].UUID, DeploymentID: rows[i].DeploymentID, NodeID: rows[i].NodeID, GPUs: gpus, Live: true})
		}
	}
	return out, nil
}
