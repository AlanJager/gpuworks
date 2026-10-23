package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func TestUsageAddsUpRollsUpAndRetains(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	// 2026-10-22 in Shanghai runs 10-21 16:00 to 10-22 16:00 UTC.
	h := func(day, hour int) int64 { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC).UnixMilli() }
	key := rollup.UsageKey{HourMS: h(21, 16), APIKeyID: 3, UserID: 1, DeploymentID: 17, Source: uxrecord.SourceAPI}
	one := rollup.Usage{Requests: 2, NoUsage: 1, PromptTokens: 1000, CompletionTokens: 40, CachedTokens: 800, CacheReported: 1, CacheReportedPrompt: 1000}
	// Two flushes of the same hour add up.
	if err := st.AddUsage(ctx, map[rollup.UsageKey]rollup.Usage{key: one}); err != nil {
		t.Fatal(err)
	}
	late := key
	late.HourMS = h(22, 15) // the day's last hour
	before := key
	before.HourMS = h(21, 15) // the 21st in Shanghai
	pg := rollup.UsageKey{HourMS: h(22, 3), UserID: 1, DeploymentID: 17, Source: uxrecord.SourcePlayground}
	if err := st.AddUsage(ctx, map[rollup.UsageKey]rollup.Usage{key: one, late: one, before: one, pg: {Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	var row UsageHourly
	if err := st.db.Where("hour_ms = ? AND api_key_id = 3", key.HourMS).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Requests != 4 || row.PromptTokens != 2000 || row.CachedTokens != 1600 || row.CacheReportedPrompt != 2000 || row.Source != 0 {
		t.Fatalf("hour row = %+v", row)
	}
	if err := st.RollupUsageDay(ctx, 20261022, shanghai); err != nil {
		t.Fatal(err)
	}
	// Rolling again changes nothing.
	if err := st.RollupUsageDay(ctx, 20261022, shanghai); err != nil {
		t.Fatal(err)
	}
	var days []UsageDaily
	if err := st.db.Order("source").Find(&days).Error; err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[0].Day != 20261022 || days[0].Requests != 6 || days[0].PromptTokens != 3000 || days[1].Source != 1 || days[1].Requests != 1 {
		t.Fatalf("day rows = %+v", days)
	}
	if rolled, err := st.UsageDayRolled(ctx, 20261022); err != nil || !rolled {
		t.Fatalf("rolled = %v %v", rolled, err)
	}
	if rolled, _ := st.UsageDayRolled(ctx, 20261021); rolled {
		t.Fatal("an unrolled day reads rolled")
	}
	hourly, daily, err := st.UsageRowCounts(ctx)
	if err != nil || hourly != 4 || daily != 2 {
		t.Fatalf("counts = %d %d %v", hourly, daily, err)
	}
	// Retention drops the hours before the cutoff and the days before its day.
	n, err := st.RetainUsage(ctx, h(22, 0), 20261023)
	if err != nil || n != 2+2+1 {
		t.Fatalf("retained %d %v", n, err)
	}
	hourly, daily, _ = st.UsageRowCounts(ctx)
	if hourly != 2 || daily != 0 {
		t.Fatalf("after retention = %d %d", hourly, daily)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.RetainUsage(cancelled, h(23, 0), 20261023); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retention = %v", err)
	}
}

// The usage index serves a per-key range without scanning the table.
func TestUsageKeyQueryUsesTheIndex(t *testing.T) {
	st := newTestStore(t)
	var plan []struct {
		ID, Parent, Notused int
		Detail              string
	}
	if err := st.db.Raw(`EXPLAIN QUERY PLAN SELECT SUM(prompt_tokens) FROM usage_hourly WHERE api_key_id = ? AND hour_ms >= ? AND hour_ms < ?`, 3, 0, 1).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 || !strings.Contains(plan[0].Detail, "idx_usage_hourly_key") {
		t.Fatalf("plan = %+v", plan)
	}
	if err := st.db.Raw(`EXPLAIN QUERY PLAN SELECT SUM(prompt_tokens) FROM usage_hourly WHERE hour_ms >= ? AND hour_ms < ? GROUP BY api_key_id`, 0, 1).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	for _, p := range plan {
		if strings.Contains(p.Detail, "SCAN usage_hourly") && !strings.Contains(p.Detail, "USING") {
			t.Fatalf("a range over hours scans the table: %+v", plan)
		}
	}
}

func TestUsageRollupStateStartsOnce(t *testing.T) {
	st := newTestStore(t)
	first := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	state, err := st.UsageRollupState(first)
	if err != nil || state.LiveSinceMS != first.UnixMilli() || state.BackfillThroughMS != first.UnixMilli() || state.BackfillDone {
		t.Fatalf("first = %+v %v", state, err)
	}
	if err := st.AddUsageBackfill(context.Background(), nil, first.Add(-time.Hour).UnixMilli(), true); err != nil {
		t.Fatal(err)
	}
	// A restart keeps when live accumulation began.
	again, err := st.UsageRollupState(first.Add(time.Hour))
	if err != nil || again.LiveSinceMS != first.UnixMilli() || !again.BackfillDone || again.BackfillThroughMS != first.Add(-time.Hour).UnixMilli() {
		t.Fatalf("again = %+v %v", again, err)
	}
}

func TestDeletingADeploymentKeepsItsNames(t *testing.T) {
	st := newTestStore(t)
	dep, err := st.CreateDeployment(&Deployment{Name: "glm", ServedName: "nvidia/GLM", State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteDeployment(dep.ID); err != nil {
		t.Fatal(err)
	}
	gone, err := st.DeletedDeployments([]uint{dep.ID, 999})
	if err != nil || len(gone) != 1 || gone[dep.ID].Name != "glm" || gone[dep.ID].ServedName != "nvidia/GLM" || gone[dep.ID].DeletedAt.IsZero() {
		t.Fatalf("deleted = %+v %v", gone, err)
	}
	if err := st.DeleteDeployment(dep.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if none, err := st.DeletedDeployments(nil); err != nil || len(none) != 0 {
		t.Fatal("empty lookup")
	}
}

func TestOperationsUsageMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 170)
	tables := []string{"usage_hourly", "usage_daily", "usage_rolled_days", "usage_rollup_state", "deleted_deployments"}
	db := open("operations-usage")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("%s was not created", table)
		}
	}
	if !db.Migrator().HasIndex(&UsageHourly{}, "idx_usage_hourly_key") || !db.Migrator().HasIndex(&UsageDaily{}, "idx_usage_daily_key") {
		t.Fatal("key indexes missing")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 170).Error; err != nil || marker.Name != "operations_usage" {
		t.Fatalf("marker = %+v %v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 170").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("operations-usage-rollback")
	want := errors.New("injected operations usage failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	for _, table := range tables {
		if rollback.Migrator().HasTable(table) {
			t.Fatalf("%s survived the failed migration", table)
		}
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 170).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d %v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasTable("usage_hourly") {
		t.Fatalf("restart after rollback: %v", err)
	}
}

// The card-time sampler's read: awake running or starting instances with
// their cards and their deployment's dGPU limit, and every card of an online
// node.
func TestGPUTimeSnapshotReadsTheHeldCards(t *testing.T) {
	st := newTestStore(t)
	online, err := st.RegisterNode("n-online", HashToken("a"), "online")
	if err != nil {
		t.Fatal(err)
	}
	offline, err := st.RegisterNode("n-offline", HashToken("b"), "offline")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.db.Model(&Node{}).Where("id = ?", online.ID).Update("state", NodeStateOnline).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.db.Model(&Node{}).Where("id = ?", offline.ID).Update("state", NodeStateOffline).Error; err != nil {
		t.Fatal(err)
	}
	for _, g := range []GPU{
		{NodeID: online.ID, Ident: "GPU-a", Index: 0, Name: "RTX 6000D", MemoryTotal: 84 << 30},
		{NodeID: online.ID, Ident: "GPU-b", Index: 1, Name: "RTX 6000D", MemoryTotal: 84 << 30, Ownership: "passthrough"},
		{NodeID: offline.ID, Ident: "GPU-c", Index: 0, Name: "PPU 810E", MemoryTotal: 96 << 30},
	} {
		g := g
		if err := st.db.Create(&g).Error; err != nil {
			t.Fatal(err)
		}
	}
	whole, err := st.CreateDeployment(&Deployment{Name: "whole", ServedName: "whole", State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	dgpu, err := st.CreateDeployment(&Deployment{Name: "dgpu", ServedName: "dgpu", State: DeployActive, VRAMLimitBytes: 21 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []Instance{
		{UUID: "i-run", DeploymentID: whole.ID, NodeID: online.ID, State: proto.InstRunning, GPUIndexes: "[0]", GPUIdents: `["GPU-a"]`},
		{UUID: "i-dgpu", DeploymentID: dgpu.ID, NodeID: online.ID, State: proto.InstStarting, GPUIndexes: "[0]", GPUIdents: `["GPU-a"]`},
		{UUID: "i-sleep", DeploymentID: whole.ID, NodeID: online.ID, State: proto.InstRunning, GPUIndexes: "[0]", GPUIdents: `["GPU-a"]`, SleepLevel: 1},
		{UUID: "i-stopped", DeploymentID: whole.ID, NodeID: online.ID, State: proto.InstStopped, GPUIndexes: "[0]", GPUIdents: `["GPU-a"]`},
		{UUID: "i-cpu", DeploymentID: whole.ID, NodeID: online.ID, State: proto.InstRunning},
	} {
		in := in
		if _, err := st.CreateInstance(&in); err != nil {
			t.Fatal(err)
		}
		if in.SleepLevel > 0 {
			if err := st.db.Model(&Instance{}).Where("uuid = ?", in.UUID).Update("sleep_level", in.SleepLevel).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	snap, err := st.GPUTimeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Cards) != 2 {
		t.Fatalf("cards of online nodes = %+v", snap.Cards)
	}
	byDep := map[uint]GPUHolding{}
	for _, h := range snap.Holdings {
		byDep[h.DeploymentID] = h
	}
	if len(snap.Holdings) != 2 || byDep[whole.ID].VRAMLimitBytes != 0 || byDep[dgpu.ID].VRAMLimitBytes != 21<<30 ||
		len(byDep[whole.ID].Idents) != 1 || byDep[whole.ID].Idents[0] != "GPU-a" || byDep[whole.ID].NodeID != online.ID {
		t.Fatalf("holdings = %+v", snap.Holdings)
	}
}
