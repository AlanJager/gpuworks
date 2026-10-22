package store

import "fmt"

// Timeline reads for instance and deployment events: newest first, cursor
// paginated, and folded so a run of identical milestones reads as one entry.
//
// Folding happens on read rather than by rewriting history. Databases written
// before progress stopped being an event hold runs of tens of thousands of rows
// that differ only in a byte counter; deleting them would destroy history to
// fix a display problem, and migrating them would rewrite it. Retention ages
// them out on its own schedule, and until then the timeline stays readable.

// eventTimelineDefaultLimit and eventTimelineMaxLimit bound one page. The
// maximum matches the fixed limit the endpoint used to return in full, so a
// client that asks for everything gets no less than it used to.
const (
	eventTimelineDefaultLimit = 100
	eventTimelineMaxLimit     = 500
)

// eventFoldKey is what makes two adjacent rows the same milestone: the same
// instance, kind, code, state and phase. Everything else about a progress row -
// its message, its byte counters - is a sample of that milestone, not another
// one.
//
// Data is free text on rows written before events carried codes, and
// json_extract raises on malformed JSON, so such a row falls back to comparing
// the raw text. That keeps distinct historical entries distinct instead of
// silently collapsing them into one.
const eventFoldKey = `COALESCE(instance_uuid,'') || char(31) || COALESCE(kind,'') || char(31) || COALESCE(code,'') || char(31) ||
	CASE WHEN json_valid(data)
		THEN COALESCE(json_extract(data,'$.state'),'') || char(31) || COALESCE(json_extract(data,'$.phase'),'')
		ELSE COALESCE(data,'')
	END`

// The window is computed over the whole scope, never over the page. A row is
// the end of its run if the next row by id is a different milestone, which is a
// property of the row itself - so it does not change as the cursor moves, and a
// run that straddles a page boundary can neither be returned twice nor lost.
const eventTimelineQuery = `SELECT id, instance_id, instance_uuid, deployment_id, kind, code, message, data, created_at FROM (
	SELECT id, instance_id, instance_uuid, deployment_id, kind, code, message, data, created_at, fold_key,
		LEAD(fold_key) OVER (ORDER BY id) AS next_fold_key
	FROM (
		SELECT id, instance_id, instance_uuid, deployment_id, kind, code, message, data, created_at,
			` + eventFoldKey + ` AS fold_key
		FROM instance_events WHERE %s
	)
)
WHERE (next_fold_key IS NULL OR next_fold_key <> fold_key) AND (? = 0 OR id < ?)
ORDER BY id DESC LIMIT ?`

// NormalizeEventTimelineLimit clamps a requested page size into the supported
// range. Zero selects the default.
func NormalizeEventTimelineLimit(limit int) int {
	if limit <= 0 {
		return eventTimelineDefaultLimit
	}
	if limit > eventTimelineMaxLimit {
		return eventTimelineMaxLimit
	}
	return limit
}

// InstanceTimeline returns one page of an instance's folded timeline, newest
// first. before is an exclusive event id cursor; 0 starts at the newest entry.
//
// It is addressed by UUID, not by loading the instance first: the history
// outlives the instance row, and requiring the row to exist would hide exactly
// the events a deleted deployment is diagnosed from.
func (s *Store) InstanceTimeline(uuid string, before uint, limit int) ([]InstanceEvent, error) {
	if uuid == "" {
		return nil, nil
	}
	return s.eventTimeline("instance_uuid = ?", uuid, before, limit)
}

// DeploymentTimeline returns one page of a deployment's folded timeline,
// including entries recorded before any instance existed.
func (s *Store) DeploymentTimeline(deploymentID uint, before uint, limit int) ([]InstanceEvent, error) {
	if deploymentID == 0 {
		return nil, nil
	}
	return s.eventTimeline("deployment_id = ?", deploymentID, before, limit)
}

func (s *Store) eventTimeline(scope string, scopeValue any, before uint, limit int) ([]InstanceEvent, error) {
	var evs []InstanceEvent
	// The scope predicate is one of two package constants, never caller input;
	// the scope value itself stays a bound parameter. A timeline page is a
	// pure read (InstanceTimeline/DeploymentTimeline), so it goes through the
	// read pool like the other hot list/detail queries (see Store.reader).
	query := fmt.Sprintf(eventTimelineQuery, scope)
	err := s.reader().Raw(query, scopeValue, before, before, NormalizeEventTimelineLimit(limit)).Scan(&evs).Error
	return evs, err
}
