package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// explainUsage is the query plan of a usage aggregation (tests).
func (s *Store) explainUsage(ctx context.Context, hourly bool, f UsageFilter) (string, error) {
	r := usageHours(0, 1)
	if !hourly {
		r = usageDays(0, 1)
	}
	stmt := f.where(s.reader().Session(&gorm.Session{DryRun: true}).Table(r.table).
		Where(r.column+" >= ? AND "+r.column+" < ?", r.from, r.to)).
		Select("api_key_id, user_id, deployment_id, source, " + usageSums).
		Group("api_key_id, user_id, deployment_id, source").Find(&[]UsageGroup{}).Statement
	var rows []struct {
		ID, Parent, Notused int
		Detail              string
	}
	if err := s.reader().WithContext(ctx).Raw("EXPLAIN QUERY PLAN "+stmt.SQL.String(), stmt.Vars...).Scan(&rows).Error; err != nil {
		return "", err
	}
	var details []string
	for _, r := range rows {
		details = append(details, r.Detail)
	}
	if len(details) == 0 {
		return "", fmt.Errorf("no plan")
	}
	return strings.Join(details, "; "), nil
}

func TestUsageQueriesSumBucketsAndGroups(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	h := func(hour int) int64 { return time.Date(2026, 10, 22, hour, 0, 0, 0, time.UTC).UnixMilli() }
	u := rollup.Usage{Requests: 1, PromptTokens: 100, CachedTokens: 50, CacheReported: 1, CacheReportedPrompt: 100}
	delta := map[rollup.UsageKey]rollup.Usage{
		{HourMS: h(1), APIKeyID: 3, UserID: 1, DeploymentID: 17, Source: uxrecord.SourceAPI}:        u,
		{HourMS: h(2), APIKeyID: 3, UserID: 1, DeploymentID: 17, Source: uxrecord.SourceAPI}:        u,
		{HourMS: h(2), APIKeyID: 3, UserID: 1, DeploymentID: 18, Source: uxrecord.SourceAPI}:        u,
		{HourMS: h(2), APIKeyID: 0, UserID: 1, DeploymentID: 17, Source: uxrecord.SourcePlayground}: u,
		{HourMS: h(5), APIKeyID: 4, UserID: 1, DeploymentID: 17, Source: uxrecord.SourceAPI}:        u,
	}
	if err := st.AddUsage(ctx, delta); err != nil {
		t.Fatal(err)
	}
	buckets, err := st.UsageHourBuckets(ctx, h(0), h(5), UsageFilter{})
	if err != nil || len(buckets) != 2 || buckets[0].Bucket != h(1) || buckets[1].Requests != 3 || buckets[1].PromptTokens != 300 {
		t.Fatalf("buckets = %+v %v", buckets, err)
	}
	groups, err := st.UsageHourGroups(ctx, h(0), h(24), UsageFilter{APIKeyID: 3})
	if err != nil || len(groups) != 2 {
		t.Fatalf("key 3 groups = %+v %v", groups, err)
	}
	for _, g := range groups {
		if g.DeploymentID == 17 && (g.Requests != 2 || g.FirstBucket != h(1) || g.LastBucket != h(2)) {
			t.Fatalf("key 3 on 17 = %+v", g)
		}
	}
	if g, _ := st.UsageHourGroups(ctx, h(0), h(24), UsageFilter{KeylessOnly: true}); len(g) != 1 || g[0].Source != uint8(uxrecord.SourcePlayground) {
		t.Fatalf("keyless = %+v", g)
	}
	if g, _ := st.UsageHourGroups(ctx, h(0), h(24), UsageFilter{Source: 1 + uint8(uxrecord.SourcePlayground)}); len(g) != 1 {
		t.Fatalf("playground = %+v", g)
	}
	if g, _ := st.UsageHourGroups(ctx, h(0), h(24), UsageFilter{DeploymentID: 18}); len(g) != 1 {
		t.Fatalf("deployment 18 = %+v", g)
	}
	loc := time.UTC
	if err := st.RollupUsageDay(ctx, 20261022, loc); err != nil {
		t.Fatal(err)
	}
	days, err := st.UsageDayBuckets(ctx, 20261001, 20261101, UsageFilter{})
	if err != nil || len(days) != 1 || days[0].Bucket != 20261022 || days[0].Requests != 5 {
		t.Fatalf("days = %+v %v", days, err)
	}
	if dg, _ := st.UsageDayGroups(ctx, 20261022, 20261023, UsageFilter{}); len(dg) != 4 {
		t.Fatalf("day groups = %+v", dg)
	}
	rolled, err := st.UsageRolledDays(ctx, 20261020, 20261025)
	if err != nil || len(rolled) != 1 || !rolled[20261022] {
		t.Fatalf("rolled = %v %v", rolled, err)
	}
	firstHour, firstDay, err := st.UsageEarliest(ctx)
	if err != nil || firstHour != h(1) || firstDay != 20261022 {
		t.Fatalf("earliest = %d %d %v", firstHour, firstDay, err)
	}
	byKey, err := st.UsageByKey(ctx, 20261022, 20261023, h(5), h(6))
	if err != nil || byKey[3].Requests != 3 || byKey[4].Requests != 2 || byKey[0].Requests != 0 {
		t.Fatalf("by key = %+v %v", byKey, err)
	}
}

// Ranges read the primary key, a key filter the key index: never the table.
func TestUsageQueriesUseTheirIndexes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		hourly bool
		filter UsageFilter
		want   string
	}{
		{true, UsageFilter{}, "sqlite_autoindex_usage_hourly"},
		{true, UsageFilter{APIKeyID: 3}, "idx_usage_hourly_key"},
		{false, UsageFilter{}, "sqlite_autoindex_usage_daily"},
		{false, UsageFilter{APIKeyID: 3}, "idx_usage_daily_key"},
	} {
		plan, err := st.explainUsage(ctx, tc.hourly, tc.filter)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, tc.want) {
			t.Errorf("hourly=%v filter=%+v: plan %s", tc.hourly, tc.filter, plan)
		}
	}
}
