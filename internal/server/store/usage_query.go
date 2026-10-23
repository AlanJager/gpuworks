package store

import (
	"context"

	"gorm.io/gorm"
)

// Reading the token usage back for operations analytics. Every query ranges
// over the primary key (hour_ms or day first) or, filtered by key, over the
// key index, and aggregates in SQL, so its cost is the rows in range, never
// the table.

// UsageFilter narrows a usage query; zero values do not filter.
type UsageFilter struct {
	DeploymentID uint
	// APIKeyID filters by key; KeylessOnly asks for the requests without one
	// (APIKeyID 0) instead.
	APIKeyID    uint
	KeylessOnly bool
	// Source is 1 + uxrecord.Source to filter by it, 0 for both.
	Source uint8
}

func (f UsageFilter) where(db *gorm.DB) *gorm.DB {
	if f.DeploymentID != 0 {
		db = db.Where("deployment_id = ?", f.DeploymentID)
	}
	switch {
	case f.KeylessOnly:
		db = db.Where("api_key_id = 0")
	case f.APIKeyID != 0:
		db = db.Where("api_key_id = ?", f.APIKeyID)
	}
	if f.Source != 0 {
		db = db.Where("source = ?", f.Source-1)
	}
	return db
}

const usageSums = `SUM(requests) AS requests, SUM(no_usage) AS no_usage, SUM(prompt_tokens) AS prompt_tokens,
	SUM(completion_tokens) AS completion_tokens, SUM(reasoning_tokens) AS reasoning_tokens,
	SUM(reasoning_reported) AS reasoning_reported, SUM(cached_tokens) AS cached_tokens,
	SUM(cache_reported) AS cache_reported, SUM(cache_reported_prompt) AS cache_reported_prompt`

// UsageBucket is the usage of one hour or day, every row in it summed.
type UsageBucket struct {
	// Bucket is hour_ms for hours, the YYYYMMDD day for days.
	Bucket int64
	UsageCounts
}

// UsageGroup is the usage of one key x user x deployment x source over a
// range, with the first and last bucket it was used in.
type UsageGroup struct {
	APIKeyID     uint
	UserID       uint
	DeploymentID uint
	Source       uint8
	FirstBucket  int64
	LastBucket   int64
	UsageCounts
}

// usageRange is a table and the bounds of its first key column.
type usageRange struct {
	table, column string
	from, to      int64
}

// usageHours are the hour table's rows from fromMS up to toMS.
func usageHours(fromMS, toMS int64) usageRange {
	return usageRange{"usage_hourly", "hour_ms", fromMS, toMS}
}

// usageDays are the day table's rows from fromDay up to toDay (YYYYMMDD,
// toDay excluded).
func usageDays(fromDay, toDay int32) usageRange {
	return usageRange{"usage_daily", "day", int64(fromDay), int64(toDay)}
}

func (s *Store) usageBuckets(ctx context.Context, r usageRange, f UsageFilter) ([]UsageBucket, error) {
	var out []UsageBucket
	q := f.where(s.reader().WithContext(ctx).Table(r.table).
		Where(r.column+" >= ? AND "+r.column+" < ?", r.from, r.to)).
		Select(r.column + " AS bucket, " + usageSums).Group(r.column).Order(r.column)
	err := q.Scan(&out).Error
	return out, err
}

func (s *Store) usageGroups(ctx context.Context, r usageRange, f UsageFilter) ([]UsageGroup, error) {
	var out []UsageGroup
	q := f.where(s.reader().WithContext(ctx).Table(r.table).
		Where(r.column+" >= ? AND "+r.column+" < ?", r.from, r.to)).
		Select("api_key_id, user_id, deployment_id, source, MIN(" + r.column + ") AS first_bucket, MAX(" + r.column + ") AS last_bucket, " + usageSums).
		Group("api_key_id, user_id, deployment_id, source")
	err := q.Scan(&out).Error
	return out, err
}

// UsageHourBuckets sums the hours from fromMS up to toMS, one bucket an hour.
func (s *Store) UsageHourBuckets(ctx context.Context, fromMS, toMS int64, f UsageFilter) ([]UsageBucket, error) {
	return s.usageBuckets(ctx, usageHours(fromMS, toMS), f)
}

// UsageDayBuckets sums the rolled-up days from fromDay up to toDay.
func (s *Store) UsageDayBuckets(ctx context.Context, fromDay, toDay int32, f UsageFilter) ([]UsageBucket, error) {
	return s.usageBuckets(ctx, usageDays(fromDay, toDay), f)
}

// UsageHourGroups sums each key x user x deployment x source over the hours
// from fromMS up to toMS.
func (s *Store) UsageHourGroups(ctx context.Context, fromMS, toMS int64, f UsageFilter) ([]UsageGroup, error) {
	return s.usageGroups(ctx, usageHours(fromMS, toMS), f)
}

// UsageDayGroups is UsageHourGroups over rolled-up days.
func (s *Store) UsageDayGroups(ctx context.Context, fromDay, toDay int32, f UsageFilter) ([]UsageGroup, error) {
	return s.usageGroups(ctx, usageDays(fromDay, toDay), f)
}

// UsageRolledDays are the days from fromDay up to toDay that are rolled up.
func (s *Store) UsageRolledDays(ctx context.Context, fromDay, toDay int32) (map[int32]bool, error) {
	var days []int32
	err := s.reader().WithContext(ctx).Model(&UsageRolledDay{}).
		Where("day >= ? AND day < ?", fromDay, toDay).Pluck("day", &days).Error
	out := make(map[int32]bool, len(days))
	for _, d := range days {
		out[d] = true
	}
	return out, err
}

// UsageEarliest is the oldest hour (Unix ms) and day (YYYYMMDD) the usage
// holds; 0 when that table is empty.
func (s *Store) UsageEarliest(ctx context.Context) (hourMS int64, day int32, err error) {
	db := s.reader().WithContext(ctx)
	var h, d *int64
	if err := db.Table("usage_hourly").Select("MIN(hour_ms)").Scan(&h).Error; err != nil {
		return 0, 0, err
	}
	if err := db.Table("usage_daily").Select("MIN(day)").Scan(&d).Error; err != nil {
		return 0, 0, err
	}
	if h != nil {
		hourMS = *h
	}
	if d != nil {
		day = int32(*d)
	}
	return hourMS, day, nil
}

// UsageByKey sums each key's usage over the rolled-up days from fromDay up
// to toDay plus the hours from fromHourMS up to toHourMS (the days not rolled
// up yet): the API Keys page's today and last-7-days columns in two range
// reads.
func (s *Store) UsageByKey(ctx context.Context, fromDay, toDay int32, fromHourMS, toHourMS int64) (map[uint]UsageCounts, error) {
	out := map[uint]UsageCounts{}
	for _, r := range []usageRange{usageDays(fromDay, toDay), usageHours(fromHourMS, toHourMS)} {
		if r.from >= r.to {
			continue
		}
		var rows []struct {
			APIKeyID uint
			UsageCounts
		}
		if err := s.reader().WithContext(ctx).Table(r.table).
			Where(r.column+" >= ? AND "+r.column+" < ? AND api_key_id <> 0", r.from, r.to).
			Select("api_key_id, " + usageSums).Group("api_key_id").Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			cur := out[row.APIKeyID]
			sum := cur.Rollup()
			sum.Add(row.UsageCounts.Rollup())
			out[row.APIKeyID] = UsageCounts(sum)
		}
	}
	return out, nil
}

// UsageByDeployment sums each deployment's usage over the hours from fromMS
// up to toMS: the card time's tokens, without the per-key groups.
func (s *Store) UsageByDeployment(ctx context.Context, fromMS, toMS int64, f UsageFilter) (map[uint]UsageCounts, error) {
	return s.usageByDeployment(ctx, usageHours(fromMS, toMS), f)
}

// UsageDayByDeployment is UsageByDeployment over rolled-up days.
func (s *Store) UsageDayByDeployment(ctx context.Context, fromDay, toDay int32, f UsageFilter) (map[uint]UsageCounts, error) {
	return s.usageByDeployment(ctx, usageDays(fromDay, toDay), f)
}

func (s *Store) usageByDeployment(ctx context.Context, r usageRange, f UsageFilter) (map[uint]UsageCounts, error) {
	var rows []struct {
		DeploymentID uint
		UsageCounts
	}
	err := f.where(s.reader().WithContext(ctx).Table(r.table).
		Where(r.column+" >= ? AND "+r.column+" < ?", r.from, r.to)).
		Select("deployment_id, " + usageSums).Group("deployment_id").Scan(&rows).Error
	out := make(map[uint]UsageCounts, len(rows))
	for _, row := range rows {
		out[row.DeploymentID] = row.UsageCounts
	}
	return out, err
}
