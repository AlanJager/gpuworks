package analytics

import (
	"context"
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// UsageFilter narrows the token usage; it is store.UsageFilter.
type UsageFilter = store.UsageFilter

func countsDTO(c store.UsageCounts) dto.UsageCounts {
	return dto.UsageCounts{Requests: c.Requests, NoUsage: c.NoUsage, PromptTokens: c.PromptTokens,
		CompletionTokens: c.CompletionTokens, ReasoningTokens: c.ReasoningTokens, ReasoningReported: c.ReasoningReported,
		CachedTokens: c.CachedTokens, CacheReported: c.CacheReported, CacheReportedPrompt: c.CacheReportedPrompt}
}

func addCounts(a *store.UsageCounts, b store.UsageCounts) {
	sum := a.Rollup()
	sum.Add(b.Rollup())
	*a = store.UsageCounts(sum)
}

type usageGroupKey struct {
	key, user, dep uint
	source         uint8
}

type usageAcc struct {
	store.UsageCounts
	first, last int64
}

// usageData is the usage of a span: per bucket and per group.
type usageData struct {
	buckets []store.UsageCounts
	groups  map[usageGroupKey]*usageAcc
}

func (s *Service) usageData(ctx context.Context, sp *span, f UsageFilter) (*usageData, error) {
	out := &usageData{buckets: make([]store.UsageCounts, len(sp.buckets)), groups: map[usageGroupKey]*usageAcc{}}
	addGroups := func(groups []store.UsageGroup, bucketOf func(int64) int64) {
		for _, g := range groups {
			k := usageGroupKey{g.APIKeyID, g.UserID, g.DeploymentID, g.Source}
			acc := out.groups[k]
			first, last := bucketOf(g.FirstBucket), bucketOf(g.LastBucket)
			if acc == nil {
				acc = &usageAcc{first: first, last: last}
				out.groups[k] = acc
			}
			addCounts(&acc.UsageCounts, g.UsageCounts)
			acc.first, acc.last = min(acc.first, first), max(acc.last, last)
		}
	}
	if !sp.daily {
		buckets, err := s.Store.UsageHourBuckets(ctx, sp.fromMS, sp.toMS, f)
		if err != nil {
			return nil, err
		}
		for _, b := range buckets {
			if i := sp.position(b.Bucket); i >= 0 {
				out.buckets[i] = b.UsageCounts
			}
		}
		groups, err := s.Store.UsageHourGroups(ctx, sp.fromMS, sp.toMS, f)
		if err != nil {
			return nil, err
		}
		addGroups(groups, func(b int64) int64 { return b })
		return out, nil
	}
	dayStart := func(d int64) int64 {
		start, _ := rollup.Day(d).Bounds(sp.loc)
		return start
	}
	from, to := int32(sp.days[0]), int32(sp.lastDay())
	buckets, err := s.Store.UsageDayBuckets(ctx, from, to, f)
	if err != nil {
		return nil, err
	}
	for _, b := range buckets {
		if i := sp.position(b.Bucket); i >= 0 {
			out.buckets[i] = b.UsageCounts
		}
	}
	groups, err := s.Store.UsageDayGroups(ctx, from, to, f)
	if err != nil {
		return nil, err
	}
	addGroups(groups, dayStart)
	rolledDays, err := s.Store.UsageRolledDays(ctx, from, to)
	if err != nil {
		return nil, err
	}
	rolled := make(map[rollup.Day]bool, len(rolledDays))
	for d := range rolledDays {
		rolled[rollup.Day(d)] = true
	}
	for _, d := range sp.unrolled(rolled, s.now()) {
		hFrom, hTo := sp.hoursOf(d)
		hours, err := s.Store.UsageHourBuckets(ctx, hFrom, hTo, f)
		if err != nil {
			return nil, err
		}
		i := sp.position(int64(d))
		for _, h := range hours {
			addCounts(&out.buckets[i], h.UsageCounts)
		}
		groups, err := s.Store.UsageHourGroups(ctx, hFrom, hTo, f)
		if err != nil {
			return nil, err
		}
		start, _ := d.Bounds(sp.loc)
		addGroups(groups, func(int64) int64 { return start })
	}
	return out, nil
}

// usageByDeployment sums each deployment's usage over a span as usageData
// reads it: the rolled-up days, and the hours of the days not rolled up.
func (s *Service) usageByDeployment(ctx context.Context, sp *span, f UsageFilter) (map[uint]store.UsageCounts, error) {
	if !sp.daily {
		return s.Store.UsageByDeployment(ctx, sp.fromMS, sp.toMS, f)
	}
	from, to := int32(sp.days[0]), int32(sp.lastDay())
	out, err := s.Store.UsageDayByDeployment(ctx, from, to, f)
	if err != nil {
		return nil, err
	}
	rolledDays, err := s.Store.UsageRolledDays(ctx, from, to)
	if err != nil {
		return nil, err
	}
	rolled := make(map[rollup.Day]bool, len(rolledDays))
	for d := range rolledDays {
		rolled[rollup.Day(d)] = true
	}
	for _, d := range sp.unrolled(rolled, s.now()) {
		hFrom, hTo := sp.hoursOf(d)
		hours, err := s.Store.UsageByDeployment(ctx, hFrom, hTo, f)
		if err != nil {
			return nil, err
		}
		for dep, c := range hours {
			cur := out[dep]
			addCounts(&cur, c)
			out[dep] = cur
		}
	}
	return out, nil
}

// Usage answers the token usage over a range.
func (s *Service) Usage(ctx context.Context, q Query, f UsageFilter) (dto.UsageAnalytics, error) {
	sp, err := s.resolve(q)
	if err != nil {
		return dto.UsageAnalytics{}, err
	}
	data, err := s.usageData(ctx, sp, f)
	if err != nil {
		return dto.UsageAnalytics{}, err
	}
	out := dto.UsageAnalytics{Series: make([]dto.UsageCounts, len(data.buckets)), Rows: []dto.UsageRow{}}
	var total store.UsageCounts
	for i, b := range data.buckets {
		out.Series[i] = countsDTO(b)
		addCounts(&total, b)
	}
	out.Totals = countsDTO(total)
	keys, err := s.Store.ListAPIKeys()
	if err != nil {
		return out, err
	}
	byKey := make(map[uint]store.APIKey, len(keys))
	for _, k := range keys {
		byKey[k.ID] = k
	}
	deps, users := map[uint]bool{}, map[uint]bool{}
	for k := range data.groups {
		deps[k.dep] = true
		users[k.user] = true
	}
	depNames, err := s.deploymentNames(deps)
	if err != nil {
		return out, err
	}
	userList := make([]uint, 0, len(users))
	for id := range users {
		userList = append(userList, id)
	}
	userNames, err := s.Store.UserNames(userList)
	if err != nil {
		return out, err
	}
	for k, acc := range data.groups {
		row := dto.UsageRow{APIKeyID: k.key, UserID: k.user, UserName: userNames[k.user], DeploymentID: k.dep,
			Source: sourceName(k.source), FirstMS: acc.first, LastMS: acc.last, UsageCounts: countsDTO(acc.UsageCounts)}
		if k.key != 0 {
			if key, ok := byKey[k.key]; ok {
				row.APIKeyName, row.APIKeyPrefix, row.APIKeyRevoked = key.Name, key.Prefix, key.RevokedAt != nil
			} else {
				row.APIKeyDeleted = true
			}
		}
		if d, ok := depNames[k.dep]; ok {
			row.DeploymentName, row.ServedName, row.DeploymentDeleted = d.Name, d.ServedName, d.Deleted
		}
		out.Rows = append(out.Rows, row)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if a, b := out.Rows[i].PromptTokens, out.Rows[j].PromptTokens; a != b {
			return a > b
		}
		return out.Rows[i].APIKeyID < out.Rows[j].APIKeyID || (out.Rows[i].APIKeyID == out.Rows[j].APIKeyID && out.Rows[i].DeploymentID < out.Rows[j].DeploymentID)
	})
	earliestHour, earliestDay, err := s.Store.UsageEarliest(ctx)
	if err != nil {
		return out, err
	}
	earliest := earliestHour
	if sp.daily && earliestDay != 0 {
		start, _ := rollup.Day(earliestDay).Bounds(sp.loc)
		if earliest == 0 || start < earliest {
			earliest = start
		}
	}
	out.Range = sp.rangeDTO(s.Retention().TimeZone, earliest, s.LiveSinceMS)
	return out, nil
}

func sourceName(source uint8) string {
	return uxrecord.Source(source).String()
}

// KeyUsage is every key's tokens today and over the last 7 days.
func (s *Service) KeyUsage(ctx context.Context) (dto.KeyUsageList, error) {
	loc, err := s.Retention().Location()
	if err != nil {
		loc = time.UTC
	}
	now := s.now()
	today := rollup.DayOf(now, loc)
	weekStart := rollup.DayOf(now.AddDate(0, 0, -6), loc)
	todayFrom, todayTo := today.HoursOf(loc)
	todayCounts, err := s.Store.UsageByKey(ctx, 0, 0, todayFrom, todayTo)
	if err != nil {
		return dto.KeyUsageList{}, err
	}
	week, err := s.Store.UsageByKey(ctx, int32(weekStart), int32(today), 0, 0)
	if err != nil {
		return dto.KeyUsageList{}, err
	}
	// The week's days not rolled up yet (yesterday just after midnight, a
	// day the server was down at the end of) come from their hours.
	rolled, err := s.Store.UsageRolledDays(ctx, int32(weekStart), int32(today))
	if err != nil {
		return dto.KeyUsageList{}, err
	}
	for d := weekStart; d < today; d = d.Next() {
		if rolled[int32(d)] {
			continue
		}
		from, to := d.HoursOf(loc)
		hours, err := s.Store.UsageByKey(ctx, 0, 0, from, to)
		if err != nil {
			return dto.KeyUsageList{}, err
		}
		for id, c := range hours {
			cur := week[id]
			addCounts(&cur, c)
			week[id] = cur
		}
	}
	for id, c := range todayCounts {
		cur := week[id]
		addCounts(&cur, c)
		week[id] = cur
	}
	keys, err := s.Store.ListAPIKeys()
	if err != nil {
		return dto.KeyUsageList{}, err
	}
	out := dto.KeyUsageList{Items: []dto.KeyUsage{}, TimeZone: s.Retention().TimeZone}
	for _, k := range keys {
		w, ok := week[k.ID]
		if !ok {
			continue
		}
		out.Items = append(out.Items, dto.KeyUsage{Prefix: k.Prefix, Today: countsDTO(todayCounts[k.ID]), Last7Days: countsDTO(w)})
	}
	return out, nil
}
