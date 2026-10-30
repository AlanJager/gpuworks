package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
)

// The fleet reads must each be a range of an index, or a pass over a small
// table, never a sort of a large one: the overview runs them on every rebuild.
func TestFleetLedgerQueryPlans(t *testing.T) {
	s := newTestStore(t)
	db := s.DB()
	type query struct {
		q    *gorm.DB
		dest any
	}
	queries := map[string]query{
		"live instances":    {liveInstancesQuery(db), &[]Instance{}},
		"yield events":      {yieldEventsQuery(db, 100), &[]InstanceEvent{}},
		"residency of deps": {db.Model(&Instance{}).Select("ID").Where("deployment_id IN ?", []uint{1, 2}), &[]Instance{}},
		"window start":      {residencyBoundaryQuery(db, time.Now()), &[]InstanceEvent{}},
	}
	for name, one := range queries {
		stmt := one.q.Session(&gorm.Session{DryRun: true}).Find(one.dest).Statement
		var plan []struct{ Detail string }
		if err := db.Raw("EXPLAIN QUERY PLAN "+stmt.SQL.String(), stmt.Vars...).Scan(&plan).Error; err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var text []string
		for _, row := range plan {
			text = append(text, row.Detail)
		}
		joined := strings.Join(text, " | ")
		t.Logf("%s: %s", name, joined)
		if !strings.Contains(joined, "USING INDEX") && !strings.Contains(joined, "USING COVERING INDEX") {
			t.Errorf("%s reads without an index: %s", name, joined)
		}
		if strings.Contains(joined, "TEMP B-TREE") {
			t.Errorf("%s sorts: %s", name, joined)
		}
	}
	// The deployments, which the overview names occupants by, are read whole:
	// one row per deployment, in primary key order, with four columns.
	stmt := ledgerDeploymentsQuery(db).Session(&gorm.Session{DryRun: true}).Find(&[]Deployment{}).Statement
	if sql := stmt.SQL.String(); strings.Contains(sql, "quick_params") || strings.Contains(sql, "*") || strings.Contains(strings.ToUpper(sql), "ORDER BY") {
		t.Errorf("deployments are read with more than the four columns, or sorted: %s", sql)
	}
}

func TestLiveInstancesAreEveryInstanceNotStopped(t *testing.T) {
	s, node, ids := seedResidency(t)
	for state, uuid := range map[string]string{
		proto.InstRunning: "up", proto.InstStarting: "starting", proto.InstStopped: "gone",
		proto.InstFailed: "broke", proto.InstPendingRemoval: "leaving",
	} {
		if err := s.DB().Create(&Instance{UUID: uuid, DeploymentID: ids["alpha"], NodeID: node.ID, State: state, GPUIndexes: "[0]"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	live, err := s.LiveInstances()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, in := range live {
		got[in.UUID] = true
	}
	if len(got) != 4 || got["gone"] || !got["up"] || !got["starting"] || !got["broke"] || !got["leaving"] {
		t.Fatalf("live instances = %v, want all but the stopped one", got)
	}
}

func TestLedgerDeploymentsAndModels(t *testing.T) {
	s, _, ids := seedResidency(t)
	deployments, err := s.LedgerDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 2 {
		t.Fatalf("deployments = %d", len(deployments))
	}
	for _, d := range deployments {
		if d.Name == "" || d.Policy().Yield != servicelevel.TimeSlice || d.ModelID != 1 || d.QuickParams != "" {
			t.Errorf("deployment row = %+v, want name, level and model only", d)
		}
	}
	shared, err := s.SharedDeploymentIDs()
	if err != nil || len(shared) != 2 {
		t.Fatalf("shared deployments = %v %v", shared, err)
	}
	if _, err := s.CreateDeployment(&Deployment{ModelID: 1, Name: "always-on", ServedName: "always-on", Engine: "vllm", State: DeployActive}); err != nil {
		t.Fatal(err)
	}
	if shared, _ = s.SharedDeploymentIDs(); len(shared) != 2 || (shared[0] != ids["alpha"] && shared[1] != ids["alpha"]) {
		t.Fatalf("shared deployments after a resident one = %v", shared)
	}
	// 1100 ids span three chunks of one IN list.
	if err := s.DB().Create(&Model{Name: "m", Source: "huggingface", SourceID: "org/m", WeightBytes: 7, QuantLabel: "fp8", Dtype: "bf16"}).Error; err != nil {
		t.Fatal(err)
	}
	var wanted []uint
	for i := uint(1); i <= 1100; i++ {
		wanted = append(wanted, i)
	}
	models, err := s.LedgerModels(wanted)
	if err != nil || len(models) != 1 || models[0].WeightBytes != 7 || models[0].QuantLabel != "fp8" || models[0].Name != "" {
		t.Fatalf("models = %+v %v, want the one with its weights and format only", models, err)
	}
}

func TestYieldEventsAreTheSleepsMadeForAnother(t *testing.T) {
	s, _, ids := seedResidency(t)
	now := time.Now().UTC()
	put := func(code, data string, ago time.Duration) {
		putEvent(t, s, "u", ids["alpha"], EventKindAutomation, code, now.Add(-ago), data)
	}
	put("AUTO_SLEEP", `{"trigger":"idle"}`, 10*time.Minute)                       // idle: not a yield
	put("AUTO_SLEEP", `{"trigger":"preempt","for_deployment":2}`, 20*time.Minute) // yield
	put("YIELDED_FOR_WAKE", `{"deployment":"beta"}`, 30*time.Minute)              // yield
	put("AUTO_SLEEP", `{"trigger":"preempt","for_deployment":2}`, 3*time.Hour)    // before the window
	put("AUTO_WAKE", `{}`, 5*time.Minute)                                         // not a sleep
	events, err := s.YieldEvents(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	yields := 0
	for _, ev := range events {
		if IsYield(ev) {
			yields++
		}
	}
	if yields != 2 {
		t.Fatalf("yields = %d of %d events, want 2", yields, len(events))
	}
	if events, err = s.YieldEvents(now.Add(time.Hour)); err != nil || len(events) != 0 {
		t.Fatalf("events of a window nothing happened in = %v %v", events, err)
	}
}

// The inputs of the sharing stats are those of the named deployments' instances,
// wherever they are and whatever else runs beside them.
func TestResidencyInputsOfDeployments(t *testing.T) {
	s, node, ids := seedResidency(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, in := range []Instance{
		{UUID: "a-0", DeploymentID: ids["alpha"], NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", CreatedAt: now.Add(-time.Hour)},
		{UUID: "b-0", DeploymentID: ids["beta"], NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[1]", CreatedAt: now.Add(-time.Hour)},
	} {
		in := in
		if err := s.DB().Create(&in).Error; err != nil {
			t.Fatal(err)
		}
	}
	putEvent(t, s, "a-0", ids["alpha"], "state", instanceEventStatusUpdated, now.Add(-30*time.Minute), `{"state":"running","phase":""}`)
	insts, events, err := s.ResidencyInputsOf([]uint{ids["alpha"]}, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 1 || insts[0].UUID != "a-0" || len(events["a-0"]) == 0 || len(events["b-0"]) != 0 {
		t.Fatalf("inputs of alpha = %+v events %v, want only a-0", insts, events)
	}
	if insts, _, err = s.ResidencyInputsOf(nil, now); err != nil || insts != nil {
		t.Fatalf("inputs of no deployment = %v %v", insts, err)
	}
}

// The pending list reads the recent scheduling events only, and finds the
// same newest event per deployment that the whole-history read would.
func TestRecentLatestScheduleEvents(t *testing.T) {
	s, _, ids := seedResidency(t)
	now := time.Now().UTC() // ids grow with time, so the events are written oldest first
	putEvent(t, s, "", ids["alpha"], EventKindSchedule, "OLD", now.Add(-3*time.Hour), `{}`)
	putEvent(t, s, "", ids["beta"], EventKindSchedule, "STALE", now.Add(-2*time.Hour), `{}`)
	putEvent(t, s, "", ids["alpha"], EventKindSchedule, "FIRST", now.Add(-20*time.Minute), `{}`)
	putEvent(t, s, "", ids["alpha"], EventKindSchedule, "LAST", now.Add(-5*time.Minute), `{}`)
	putEvent(t, s, "", ids["beta"], EventKindAutomation, "NOT_SCHEDULE", now.Add(-time.Minute), `{}`)
	recent, err := s.RecentLatestScheduleEvents(now.Add(-30 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[ids["alpha"]].Code != "LAST" {
		t.Fatalf("recent latest = %v, want alpha's LAST only", recent)
	}
	all, err := s.LatestScheduleEvents()
	if err != nil || all[ids["alpha"]].Code != "LAST" || all[ids["beta"]].Code != "STALE" {
		t.Fatalf("whole-history read = %v %v", all, err)
	}
	if recent, err = s.RecentLatestScheduleEvents(now.Add(time.Hour)); err != nil || len(recent) != 0 {
		t.Fatalf("a window nothing happened in = %v %v", recent, err)
	}
	stmt := recentScheduleEventsQuery(s.DB(), 5).Session(&gorm.Session{DryRun: true}).Find(&[]InstanceEvent{}).Statement
	var plan []struct{ Detail string }
	if err := s.DB().Raw("EXPLAIN QUERY PLAN "+stmt.SQL.String(), stmt.Vars...).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	var text []string
	for _, row := range plan {
		text = append(text, row.Detail)
	}
	t.Log(strings.Join(text, " | "))
	if joined := strings.Join(text, " | "); !strings.Contains(joined, "USING") || strings.Contains(joined, "SCAN instance_events") && !strings.Contains(joined, "USING") {
		t.Errorf("recent schedule events scan the table: %s", joined)
	}
}

// Two resident deployments on one card share it too: one asleep beside the
// other is what a sharing view must show. A resident deployment alone on its
// card, or one whose instance is stopped, does not count.
func TestSharedDeploymentsIncludeResidentOnesOnOneCard(t *testing.T) {
	s := newTestStore(t)
	mk := func(name string) uint {
		t.Helper()
		dep, err := s.CreateDeployment(&Deployment{Name: name, ServedName: name, Engine: "vllm", State: DeployActive})
		if err != nil {
			t.Fatal(err)
		}
		return dep.ID
	}
	place := func(uuid string, dep, node uint, gpus string, state string) {
		t.Helper()
		if err := s.DB().Create(&Instance{UUID: uuid, DeploymentID: dep, NodeID: node, GPUIndexes: gpus, State: state}).Error; err != nil {
			t.Fatal(err)
		}
	}
	chat, asr, alone, gone := mk("chat"), mk("asr"), mk("alone"), mk("gone")
	place("chat-1", chat, 1, "[1]", "running")
	place("asr-1", asr, 1, "[0,1]", "running")
	place("alone-1", alone, 2, "[1]", "running")
	place("gone-1", gone, 2, "[1]", "stopped")
	place("other-node", gone, 3, "[0]", "stopped")
	got, err := s.SharedDeploymentIDs()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []uint{chat, asr}) && !slices.Equal(got, []uint{min(chat, asr), max(chat, asr)}) {
		t.Fatalf("shared = %v, want chat %d and asr %d (same card on node 1)", got, chat, asr)
	}
}
