package store

import (
	"sort"
	"time"

	"gorm.io/gorm"
)

// The scheduling history is the deployment timeline entries that say the
// platform or an operator changed where, whether or how many replicas of a
// deployment run: read across every deployment, newest first. It reads the
// existing instance_events rows; nothing is written for it.

// SchedulingCategory says who or what caused a scheduling history entry.
type SchedulingCategory string

const (
	// SchedulingAuto: the platform acted on its own settings or a plan: a
	// schedule, auto sleep, a waiting wake, a replica placed automatically, a
	// replica moved off a node an operator drains.
	SchedulingAuto SchedulingCategory = "auto"
	// SchedulingFailure: something went wrong and the platform reacted, or
	// gave up: a replica rebuilt, a node lost, a deployment that cannot be
	// placed, a unit out of retries.
	SchedulingFailure SchedulingCategory = "failure"
	// SchedulingManual: an operator ordered it.
	SchedulingManual SchedulingCategory = "manual"
)

// SchedulingEventCode is one timeline event code the scheduling history lists:
// the row kind it is written with, and its category.
type SchedulingEventCode struct {
	Code     string
	Kind     string
	Category SchedulingCategory
}

// Codes of the operator orders the history lists that are written by API
// handlers; the others are defined where they are recorded.
const (
	// EventReplicasChanged: an operator changed a deployment's replica count.
	// Params: from, to, actor.
	EventReplicasChanged = "REPLICAS_CHANGED"
	// EventSleepOrdered: an operator ordered sleep. Params: level, mode,
	// scope (deployment, role or replica), role, instances, actor.
	EventSleepOrdered = "SLEEP_ORDERED"
	// EventWakeOrdered: an operator woke instances that had room. Params:
	// instances, actor.
	EventWakeOrdered = "WAKE_ORDERED"
	// EventSleepHoldReleased: an operator let requests wake the deployment's
	// held sleepers without waking them. Params: instances, actor.
	EventSleepHoldReleased = "SLEEP_HOLD_RELEASED"
)

// schedulingEventCodes is THE catalogue of the scheduling history: which
// timeline codes belong to it and in which category. A code recorded
// elsewhere (controller, automation, API handlers) is listed here by its
// literal because those packages cannot be imported from the store; a test
// in the app package (scheduling_catalogue_test.go) pins every one of their
// constants to this list.
//
// The kind is part of the entry because the read goes by (kind, id): the
// instance_events index that serves it (migration 185), so a page costs a
// bounded range scan rather than a pass over the timeline.
var schedulingEventCodes = []SchedulingEventCode{
	{codeAutoSleep, EventKindAutomation, SchedulingAuto},
	{codeAutoWake, EventKindAutomation, SchedulingAuto},
	{"AUTO_SCALE", EventKindAutomation, SchedulingAuto},
	{"WAKE_WAITING", EventKindAutomation, SchedulingAuto},
	{"WAKE_WAIT_TIMEOUT", EventKindAutomation, SchedulingAuto},
	{"SCHEDULE_ACTION_FAILED", EventKindAutomation, SchedulingAuto},
	{codeYieldedForWake, EventKindAutomation, SchedulingAuto},
	{codeUnitPlaced, EventKindAutomation, SchedulingAuto},
	{codeNodeDrainMoved, EventKindAutomation, SchedulingAuto},

	{"NODE_REPLACED", EventKindAutomation, SchedulingFailure},
	{"NODE_REPLACE_REVERTED", EventKindAutomation, SchedulingFailure},
	{"NODE_REPLACE_NO_CANDIDATE", EventKindAutomation, SchedulingFailure},
	{EventReplicaRebuilt, EventKindLifecycle, SchedulingFailure},
	{EventUnitRetryExhausted, EventKindSchedule, SchedulingFailure},
	{EventDeploymentUnschedulable, EventKindSchedule, SchedulingFailure},
	{EventDeploymentRollingStalled, EventKindSchedule, SchedulingFailure},

	{EventReplicasChanged, EventKindLifecycle, SchedulingManual},
	{EventReplicaRestarted, EventKindSchedule, SchedulingManual},
	{EventUnitRetried, EventKindSchedule, SchedulingManual},
	{EventDeploymentRestarted, EventKindSchedule, SchedulingManual},
	{"PD_GROUP_RESTARTED", EventKindSchedule, SchedulingManual},
	{EventDeploymentRollingRestarted, EventKindSchedule, SchedulingManual},
	{EventSleepOrdered, EventKindLifecycle, SchedulingManual},
	{EventWakeOrdered, EventKindLifecycle, SchedulingManual},
	{EventSleepHoldReleased, EventKindLifecycle, SchedulingManual},
}

// SchedulingEventKind is the row kind a scheduling history code is written
// with; ok is false for a code the history does not list.
func SchedulingEventKind(code string) (kind string, ok bool) {
	for _, entry := range schedulingEventCodes {
		if entry.Code == code {
			return entry.Kind, true
		}
	}
	return "", false
}

// SchedulingEventCodes is the catalogue, in a stable order.
func SchedulingEventCodes() []SchedulingEventCode {
	return append([]SchedulingEventCode(nil), schedulingEventCodes...)
}

// SchedulingCategoryOf is the category of a scheduling history code; ok is
// false for a code the history does not list.
func SchedulingCategoryOf(code string) (SchedulingCategory, bool) {
	for _, entry := range schedulingEventCodes {
		if entry.Code == code {
			return entry.Category, true
		}
	}
	return "", false
}

// ValidSchedulingCategory reports whether c names a category.
func ValidSchedulingCategory(c SchedulingCategory) bool {
	return c == SchedulingAuto || c == SchedulingFailure || c == SchedulingManual
}

// SchedulingHistoryFilter selects a page of the history. Zero values select
// everything: all deployments, all categories, all times, from the newest.
type SchedulingHistoryFilter struct {
	DeploymentID uint
	Categories   []SchedulingCategory
	Since, Until time.Time
	// Before is an exclusive event id cursor; 0 starts at the newest entry.
	Before uint
	Limit  int
}

// SchedulingHistory returns one page of the history, newest first.
//
// The codes the filter selects are grouped by the kind they are written with
// and each kind is one range read of the (kind, id) index in id order, cut at
// the page size, so no pass sorts or scans the timeline; the per-kind pages
// are then merged by id. A time window is first turned into an id range
// through the created_at index, so a window long past is read as a bounded
// range rather than by walking every newer row of the kind.
func (s *Store) SchedulingHistory(f SchedulingHistoryFilter) ([]InstanceEvent, error) {
	limit := NormalizeEventTimelineLimit(f.Limit)
	ids, empty, err := s.historyIDRange(f.Since, f.Until)
	if err != nil || empty {
		return nil, err
	}
	byKind := map[string][]string{}
	for _, entry := range schedulingEventCodes {
		if len(f.Categories) > 0 && !containsCategory(f.Categories, entry.Category) {
			continue
		}
		byKind[entry.Kind] = append(byKind[entry.Kind], entry.Code)
	}
	var out []InstanceEvent
	for kind, codes := range byKind {
		var page []InstanceEvent
		if err := s.historyKindQuery(f, ids, kind, codes, limit).Find(&page).Error; err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// historyKindQuery is the read of one kind's codes: the newest rows first,
// by id, within the filter.
func (s *Store) historyKindQuery(f SchedulingHistoryFilter, ids eventIDRange, kind string, codes []string, limit int) *gorm.DB {
	query := s.reader().Where("kind = ? AND code IN ?", kind, codes)
	if f.DeploymentID != 0 {
		query = query.Where("deployment_id = ?", f.DeploymentID)
	}
	if f.Before != 0 {
		query = query.Where("id < ?", f.Before)
	}
	if ids.from != 0 {
		query = query.Where("id >= ?", ids.from)
	}
	if ids.to != 0 {
		query = query.Where("id <= ?", ids.to)
	}
	if !f.Since.IsZero() {
		query = query.Where("created_at >= ?", f.Since.UTC())
	}
	if !f.Until.IsZero() {
		query = query.Where("created_at <= ?", f.Until.UTC())
	}
	return query.Order("id desc").Limit(limit)
}

// eventIDRange bounds a read by event id; a zero end is open.
type eventIDRange struct{ from, to uint }

// eventClockSkew is how far apart in time two events may be written out of
// id order (the clock is read before the insert), with room to spare. The id
// bounds of a time window are widened by it; the created_at conditions still
// decide which rows are in.
const eventClockSkew = time.Minute

// historyIDRange turns a time window into the ids that can hold its events,
// each end one or two reads of the created_at index; empty when no event
// lies in the window.
func (s *Store) historyIDRange(since, until time.Time) (ids eventIDRange, empty bool, err error) {
	if !since.IsZero() {
		var first InstanceEvent
		res := s.reader().Select("created_at").Where("created_at >= ?", since.UTC()).Order("created_at").Limit(1).Find(&first)
		if res.Error != nil || res.RowsAffected == 0 {
			return ids, res.Error == nil, res.Error
		}
		if err := s.reader().Model(&InstanceEvent{}).Select("COALESCE(MIN(id), 0)").
			Where("created_at >= ? AND created_at < ?", first.CreatedAt, first.CreatedAt.Add(eventClockSkew)).Scan(&ids.from).Error; err != nil {
			return ids, false, err
		}
	}
	if !until.IsZero() {
		var last InstanceEvent
		res := s.reader().Select("created_at").Where("created_at <= ?", until.UTC()).Order("created_at desc").Limit(1).Find(&last)
		if res.Error != nil || res.RowsAffected == 0 {
			return ids, res.Error == nil, res.Error
		}
		if err := s.reader().Model(&InstanceEvent{}).Select("COALESCE(MAX(id), 0)").
			Where("created_at > ? AND created_at <= ?", last.CreatedAt.Add(-eventClockSkew), last.CreatedAt).Scan(&ids.to).Error; err != nil {
			return ids, false, err
		}
	}
	return ids, ids.from != 0 && ids.to != 0 && ids.from > ids.to, nil
}

func containsCategory(list []SchedulingCategory, c SchedulingCategory) bool {
	for _, item := range list {
		if item == c {
			return true
		}
	}
	return false
}
