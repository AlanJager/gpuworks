package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// cardNode is a node with two cards: a healthy one (3f) and one whose health
// the test sets (40) — the report a status loop would send.
func cardNode(t *testing.T, s *Store, uuid string) func(code string, foreign bool) {
	t.Helper()
	if _, err := s.RegisterNode(uuid, HashToken(uuid), uuid); err != nil {
		t.Fatal(err)
	}
	ok := true
	return func(code string, foreign bool) {
		t.Helper()
		second := proto.GPUInfo{Index: 1, UUID: "GPU-40", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", MemoryTotalBytes: 24 << 30,
			HealthCode: code, HealthStatus: accelerator.HealthLevel(code), Healthy: accelerator.HealthSchedulable(accelerator.HealthLevel(code))}
		if foreign {
			second.ForeignProcesses = []proto.CardProcess{{PID: 42, Name: "python3"}}
		}
		cards := []proto.GPUInfo{{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}, second}
		if err := s.UpdateNodeStatus(uuid, proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
			t.Fatal(err)
		}
	}
}

func cardOf(t *testing.T, s *Store, nodeUUID, bdf string) GPU {
	t.Helper()
	node, err := s.NodeWithGPUs(nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range node.GPUs {
		if g.PCIeBDF == bdf {
			return g
		}
	}
	t.Fatalf("no card %s on %s", bdf, nodeUUID)
	return GPU{}
}

var card40 = accelerator.CardIdent("GPU-40", "0000:40:00.0")

// An operator's stop holds whatever the reports say, and only they lift it.
// The reports neither undo it nor rewrite the row for it.
func TestAnOperatorsCardStopHoldsUntilTheyLiftIt(t *testing.T) {
	s := newTestStore(t)
	report := cardNode(t, s, "n-stop")
	report("", false)
	if _, err := s.SetCardScheduling("n-stop", card40, CardSchedulingStopped); err != nil {
		t.Fatal(err)
	}
	c := cardOf(t, s, "n-stop", "0000:40:00.0")
	if c.CanTakeWork() || !c.SchedulingStopped || fmt.Sprint(c.SchedulingActions()) != "[schedulable]" {
		t.Fatalf("stopped card = can=%v stopped=%v actions=%v", c.CanTakeWork(), c.SchedulingStopped, c.SchedulingActions())
	}
	before := c.UpdatedAt
	for i := 0; i < 3; i++ {
		report("", false)
	}
	c = cardOf(t, s, "n-stop", "0000:40:00.0")
	if c.CanTakeWork() || !c.SchedulingStopped {
		t.Fatal("a report lifted the operator's stop")
	}
	if !c.UpdatedAt.Equal(before) {
		t.Fatal("unchanged reports rewrote the stopped card's row")
	}
	if other := cardOf(t, s, "n-stop", "0000:3f:00.0"); !other.CanTakeWork() {
		t.Fatal("stopping one card stopped another")
	}
	if _, err := s.SetCardScheduling("n-stop", card40, CardSchedulingSchedulable); err != nil {
		t.Fatal(err)
	}
	if c := cardOf(t, s, "n-stop", "0000:40:00.0"); !c.CanTakeWork() || c.SchedulingStopped || c.SchedulingAllowedAt != nil {
		t.Fatalf("after lifting: %+v", c)
	}
}

// Making a card the platform judged unusable schedulable lets work onto it
// until the first report at least a minute later, which judges it afresh:
// still faulty, it is stopped again; recovered, it simply stays usable.
func TestAnOperatorsAllowanceLastsUntilAReportAMinuteLater(t *testing.T) {
	s := newTestStore(t)
	report := cardNode(t, s, "n-allow")
	report(accelerator.HealthRuntimeUnusable, false)
	if c := cardOf(t, s, "n-allow", "0000:40:00.0"); c.CanTakeWork() || fmt.Sprint(c.SchedulingActions()) != "[stopped schedulable]" {
		t.Fatalf("faulty card can=%v actions=%v", c.CanTakeWork(), c.SchedulingActions())
	}
	if _, err := s.SetCardScheduling("n-allow", card40, CardSchedulingSchedulable); err != nil {
		t.Fatal(err)
	}
	c := cardOf(t, s, "n-allow", "0000:40:00.0")
	if !c.CanTakeWork() || c.SchedulingAllowedAt == nil || c.HealthCode != accelerator.HealthRuntimeUnusable {
		t.Fatalf("allowed card = %+v, want work allowed and the fault still shown", c)
	}
	// A report within the window keeps the allowance.
	report(accelerator.HealthRuntimeUnusable, false)
	if c := cardOf(t, s, "n-allow", "0000:40:00.0"); !c.CanTakeWork() {
		t.Fatal("a report inside the window ended the allowance")
	}
	// One after it judges the card afresh.
	if err := s.db.Model(&GPU{}).Where("ident = ?", card40).Update("scheduling_allowed_at", time.Now().Add(-2*cardAllowanceWindow)).Error; err != nil {
		t.Fatal(err)
	}
	report(accelerator.HealthRuntimeUnusable, false)
	if c := cardOf(t, s, "n-allow", "0000:40:00.0"); c.CanTakeWork() || c.SchedulingAllowedAt != nil {
		t.Fatalf("after the window, still faulty = %+v, want unusable again", c)
	}

	// The same for a card held outside the platform.
	report("", true)
	if _, err := s.SetCardScheduling("n-allow", card40, CardSchedulingSchedulable); err != nil {
		t.Fatal(err)
	}
	if c := cardOf(t, s, "n-allow", "0000:40:00.0"); !c.CanTakeWork() || !c.HeldOutsidePlatform() {
		t.Fatalf("allowed foreign-held card = %+v", c)
	}
	// A card that is fine needs no allowance.
	report("", false)
	if _, err := s.SetCardScheduling("n-allow", card40, CardSchedulingSchedulable); err != nil {
		t.Fatal(err)
	}
	if c := cardOf(t, s, "n-allow", "0000:40:00.0"); c.SchedulingAllowedAt != nil || !c.CanTakeWork() {
		t.Fatalf("healthy card = %+v, want no allowance recorded", c)
	}
}

func TestSetCardSchedulingRefusals(t *testing.T) {
	s := newTestStore(t)
	report := cardNode(t, s, "n-refuse")
	report("", false)
	if _, err := s.SetCardScheduling("n-refuse", card40, "paused"); !errors.Is(err, ErrInvalidCardScheduling) {
		t.Fatalf("invalid = %v", err)
	}
	if _, err := s.SetCardScheduling("n-refuse", "pci:0000:99:00.0", CardSchedulingStopped); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown card = %v", err)
	}
	if _, err := s.SetCardScheduling("n-missing", card40, CardSchedulingStopped); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node = %v", err)
	}
	if err := s.db.Model(&GPU{}).Where("ident = ?", card40).Update("ownership", accelerator.CardPassedThrough).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetCardScheduling("n-refuse", card40, CardSchedulingSchedulable); !errors.Is(err, ErrCardNotOwned) {
		t.Fatalf("lent card = %v", err)
	}
	if c := cardOf(t, s, "n-refuse", "0000:40:00.0"); c.SchedulingActions() != nil {
		t.Fatalf("a lent card offers %v", c.SchedulingActions())
	}
}

// The fleet's capacity counts read the operator's decisions too.
func TestFleetCountsReadTheOperatorsCardDecisions(t *testing.T) {
	s := newTestStore(t)
	report := cardNode(t, s, "n-fleet")
	report(accelerator.HealthRuntimeUnusable, false)
	count := func() (cards, unusable int) {
		groups, err := s.OnlineGPUGroups()
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range groups {
			cards, unusable = cards+g.Cards, unusable+g.Unusable
		}
		return
	}
	if c, u := count(); c != 1 || u != 1 {
		t.Fatalf("faulty: cards=%d unusable=%d", c, u)
	}
	if _, err := s.SetCardScheduling("n-fleet", card40, CardSchedulingSchedulable); err != nil {
		t.Fatal(err)
	}
	if c, u := count(); c != 2 || u != 0 {
		t.Fatalf("allowed: cards=%d unusable=%d", c, u)
	}
	if _, err := s.SetCardScheduling("n-fleet", accelerator.CardIdent("GPU-3f", "0000:3f:00.0"), CardSchedulingStopped); err != nil {
		t.Fatal(err)
	}
	if c, u := count(); c != 1 || u != 1 {
		t.Fatalf("one stopped: cards=%d unusable=%d", c, u)
	}
}

func TestGPUOperatorSchedulingMigration(t *testing.T) {
	s := newTestStore(t)
	db := s.db
	m := migrationByName(t, "gpu_operator_scheduling")
	for _, c := range []string{"SchedulingStopped", "SchedulingAllowedAt"} {
		if !db.Migrator().HasColumn(&GPU{}, c) {
			t.Fatalf("%s missing after migrations", c)
		}
	}
	// Applying it again changes nothing.
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	// From before the columns: they arrive with nothing decided.
	t.Run("from an older schema", func(t *testing.T) {
		old := nodesOnlyDB(t, "")
		if err := old.Exec(`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, ident TEXT)`).Error; err != nil {
			t.Fatal(err)
		}
		if err := old.Exec(`INSERT INTO gpus (node_id, ident) VALUES (1, 'pci:0000:3f:00.0')`).Error; err != nil {
			t.Fatal(err)
		}
		if err := migrateDB(old, []migration{m}); err != nil {
			t.Fatal(err)
		}
		var row struct {
			SchedulingStopped   bool
			SchedulingAllowedAt *time.Time
		}
		if err := old.Raw(`SELECT scheduling_stopped, scheduling_allowed_at FROM gpus`).Scan(&row).Error; err != nil || row.SchedulingStopped || row.SchedulingAllowedAt != nil {
			t.Fatalf("existing card = %+v, %v", row, err)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		failing := nodesOnlyDB(t, "")
		if err := failing.Exec(`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, ident TEXT)`).Error; err != nil {
			t.Fatal(err)
		}
		if err := failing.Exec(fmt.Sprintf(`CREATE TRIGGER fail_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
			t.Fatal(err)
		}
		if err := migrateDB(failing, []migration{m}); err == nil {
			t.Fatal("injected marker failure did not fail the migration")
		}
		if failing.Migrator().HasColumn("gpus", "scheduling_stopped") {
			t.Fatal("the column survived a failed migration")
		}
	})
}
