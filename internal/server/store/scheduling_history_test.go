package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// seedHistory writes a busy timeline: thousands of per-instance state rows
// that the history must never read, and the scheduling rows it lists.
func seedHistory(t *testing.T, s *Store) {
	t.Helper()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var rows []InstanceEvent
	for i := 0; i < 5000; i++ {
		rows = append(rows, InstanceEvent{InstanceUUID: fmt.Sprintf("inst-%d", i%50), DeploymentID: uint(i%10 + 1), Kind: "state", Code: "INSTANCE_STATE_CHANGED", Data: "{}", CreatedAt: base.Add(time.Duration(i) * time.Second)})
	}
	add := func(dep uint, kind, code string, at time.Duration) {
		rows = append(rows, InstanceEvent{DeploymentID: dep, Kind: kind, Code: code, Data: `{"actor":"alice"}`, CreatedAt: base.Add(at)})
	}
	add(1, EventKindAutomation, "AUTO_SLEEP", 10*time.Hour)
	add(1, EventKindSchedule, EventReplicasChanged, 11*time.Hour)
	add(2, EventKindSchedule, EventReplicaRebuilt, 12*time.Hour)
	add(2, EventKindAutomation, "NODE_REPLACED", 13*time.Hour)
	add(3, EventKindSchedule, EventDeploymentEdited, 14*time.Hour) // configuration, not scheduling
	add(3, EventKindSchedule, EventUnitRetried, 15*time.Hour)
	if err := s.DB().CreateInBatches(rows, 500).Error; err != nil {
		t.Fatal(err)
	}
}

func historyCodes(rows []InstanceEvent) string {
	codes := make([]string, 0, len(rows))
	for _, r := range rows {
		codes = append(codes, r.Code)
	}
	return strings.Join(codes, ",")
}

func TestSchedulingHistoryFiltersAndOrders(t *testing.T) {
	s := newTestStore(t)
	seedHistory(t, s)

	all, err := s.SchedulingHistory(SchedulingHistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := historyCodes(all), "UNIT_RETRIED,NODE_REPLACED,REPLICA_REBUILT,REPLICAS_CHANGED,AUTO_SLEEP"; got != want {
		t.Fatalf("all = %s, want %s (newest first, no state rows, no config edits)", got, want)
	}

	for _, c := range []struct {
		name   string
		filter SchedulingHistoryFilter
		want   string
	}{
		{"manual", SchedulingHistoryFilter{Categories: []SchedulingCategory{SchedulingManual}}, "UNIT_RETRIED,REPLICAS_CHANGED"},
		{"failure and auto", SchedulingHistoryFilter{Categories: []SchedulingCategory{SchedulingFailure, SchedulingAuto}}, "NODE_REPLACED,REPLICA_REBUILT,AUTO_SLEEP"},
		{"deployment", SchedulingHistoryFilter{DeploymentID: 2}, "NODE_REPLACED,REPLICA_REBUILT"},
		{"since", SchedulingHistoryFilter{Since: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}, "UNIT_RETRIED,NODE_REPLACED,REPLICA_REBUILT"},
		{"until", SchedulingHistoryFilter{Until: time.Date(2026, 10, 1, 11, 30, 0, 0, time.UTC)}, "REPLICAS_CHANGED,AUTO_SLEEP"},
		{"limit", SchedulingHistoryFilter{Limit: 2}, "UNIT_RETRIED,NODE_REPLACED"},
	} {
		got, err := s.SchedulingHistory(c.filter)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if historyCodes(got) != c.want {
			t.Errorf("%s = %s, want %s", c.name, historyCodes(got), c.want)
		}
	}

	// The cursor continues exactly after the last row of the previous page.
	first, err := s.SchedulingHistory(SchedulingHistoryFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.SchedulingHistory(SchedulingHistoryFilter{Limit: 2, Before: first[len(first)-1].ID})
	if err != nil || historyCodes(next) != "REPLICA_REBUILT,REPLICAS_CHANGED" {
		t.Fatalf("second page = %s %v", historyCodes(next), err)
	}
}

func TestSchedulingCatalogue(t *testing.T) {
	seen := map[string]bool{}
	for _, entry := range SchedulingEventCodes() {
		if seen[entry.Code] {
			t.Errorf("%s is listed twice", entry.Code)
		}
		seen[entry.Code] = true
		if !ValidSchedulingCategory(entry.Category) {
			t.Errorf("%s has category %q", entry.Code, entry.Category)
		}
		if entry.Kind != EventKindSchedule && entry.Kind != EventKindAutomation {
			t.Errorf("%s has kind %q; the read serves only the schedule and automation kinds", entry.Code, entry.Kind)
		}
	}
	if c, ok := SchedulingCategoryOf("NODE_REPLACED"); !ok || c != SchedulingFailure {
		t.Fatalf("NODE_REPLACED = %q %v", c, ok)
	}
	if _, ok := SchedulingCategoryOf("INSTANCE_STATE_CHANGED"); ok {
		t.Fatal("a state change is not a scheduling entry")
	}
}

// Every shape of the read must be served by an index range, in id order: a
// plan that scans the table or sorts a temp b-tree would cost a pass over the
// timeline on every page of the scheduling center.
func TestSchedulingHistoryQueryPlans(t *testing.T) {
	s := newTestStore(t)
	seedHistory(t, s)
	since, until := time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	for name, f := range map[string]SchedulingHistoryFilter{
		"newest":      {},
		"cursor":      {Before: 4000},
		"deployment":  {DeploymentID: 2},
		"time window": {Since: since, Until: until},
		"everything":  {DeploymentID: 2, Before: 4000, Since: since, Until: until, Categories: []SchedulingCategory{SchedulingFailure}},
	} {
		for kind, codes := range map[string][]string{EventKindSchedule: {EventReplicaRebuilt, EventUnitRetried}, EventKindAutomation: {"AUTO_SLEEP", "NODE_REPLACED"}} {
			stmt := s.historyKindQuery(f, kind, codes, 100).Session(&gorm.Session{DryRun: true}).Find(&[]InstanceEvent{}).Statement
			var plan []struct {
				Detail string
			}
			if err := s.DB().Raw("EXPLAIN QUERY PLAN "+stmt.SQL.String(), stmt.Vars...).Scan(&plan).Error; err != nil {
				t.Fatalf("%s/%s: %v", name, kind, err)
			}
			var text []string
			for _, row := range plan {
				text = append(text, row.Detail)
			}
			joined := strings.Join(text, " | ")
			t.Logf("%s/%s: %s", name, kind, joined)
			if !strings.Contains(joined, "USING INDEX") && !strings.Contains(joined, "USING COVERING INDEX") {
				t.Errorf("%s/%s reads without an index: %s", name, kind, joined)
			}
			if strings.Contains(joined, "TEMP B-TREE") || strings.Contains(joined, "SCAN instance_events") && !strings.Contains(joined, "USING") {
				t.Errorf("%s/%s sorts or scans the table: %s", name, kind, joined)
			}
		}
	}
}
