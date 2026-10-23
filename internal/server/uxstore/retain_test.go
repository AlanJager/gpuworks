package uxstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func dayTables(t *testing.T, s *Store) []string {
	t.Helper()
	days, err := listDays(s.reader)
	if err != nil {
		t.Fatal(err)
	}
	return days
}

func TestRetainByAge(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c)) // MaxAge 7 days
	for _, d := range []int{9, 8, 7, 0} {
		s.Observe(okReq(fmt.Sprint(d), 1, ms(day0.AddDate(0, 0, -d))))
	}
	mustSync(t, s, true)
	// Cutoff is 2026-10-05 12:00: 10-03 and 10-04 ended before it; 10-05
	// still holds rows younger than the cutoff and stays whole.
	if got, want := strings.Join(dayTables(t, s), ","), "20261012,20261005"; got != want {
		t.Fatalf("days = %s, want %s", got, want)
	}
	tables := tableNames(t, s)
	for _, gone := range []string{"req_20261003", "grid_20261003", "cnt_20261003", "req_20261004"} {
		if tables[gone] {
			t.Errorf("%s survived retention", gone)
		}
	}
	st := s.Stats()
	if st.Partitions != 2 || st.PartitionsDropped != 2 {
		t.Fatalf("stats = %+v", st)
	}
	// A late record for a dropped day recreates its tables.
	s.Observe(okReq("late", 1, ms(day0.AddDate(0, 0, -8))))
	mustSync(t, s, false)
	if !tableNames(t, s)["req_20261004"] || s.Stats().Partitions != 3 {
		t.Fatalf("late record did not recreate its day: %v", s.Stats())
	}
}

// The data retention setting shortens the age bound of a running store; a
// non-positive value keeps the bound it had.
func TestSetLimitsShortensTheAgeOfARunningStore(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c))
	for _, d := range []int{3, 2, 0} {
		s.Observe(okReq(fmt.Sprint(d), 1, ms(day0.AddDate(0, 0, -d))))
	}
	mustSync(t, s, true)
	if got := len(dayTables(t, s)); got != 3 {
		t.Fatalf("days before = %d", got)
	}
	s.SetLimits(36*time.Hour, 0)
	if age, bytes := s.Limits(); age != 36*time.Hour || bytes != DefaultMaxBytes {
		t.Fatalf("limits = %v %d", age, bytes)
	}
	mustSync(t, s, true)
	if got, want := strings.Join(dayTables(t, s), ","), "20261012"; got != want {
		t.Fatalf("days = %s, want %s", got, want)
	}
	if start, ok, err := s.EarliestDayMS(); err != nil || !ok || start != ms(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("earliest = %d %v %v", start, ok, err)
	}
	if s.SizeBytes() <= 0 {
		t.Fatal("no size")
	}
}

// The writer runs retention on its own once a minute of store time has
// passed since the last pass.
func TestRetainRunsFromTheWriterLoop(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.FlushEvery = 2 * time.Millisecond
	s, _ := openTest(t, opts)
	s.Observe(okReq("old", 1, ms(day0.AddDate(0, 0, -30))))
	mustSync(t, s, false)
	time.Sleep(20 * time.Millisecond) // ticks before the minute: nothing dropped
	if got := len(dayTables(t, s)); got != 1 {
		t.Fatalf("retention ran before a minute passed: %d days", got)
	}
	c.Set(day0.Add(retainEvery))
	deadline := time.Now().Add(5 * time.Second)
	for len(dayTables(t, s)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("writer loop never ran retention")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestRetainBySizeDropsOldestButNeverToday(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.RowsPerSecond = 1 << 20
	s, _ := openTest(t, opts)
	light := func(d int) {
		for i := range 20 {
			s.Observe(okReq(fmt.Sprintf("l%d-%d", d, i), 1, ms(day0.AddDate(0, 0, -d))+int64(i)))
		}
	}
	light(2)
	light(1)
	light(0)
	mustSync(t, s, false)
	lightBytes, err := s.liveBytes()
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("p", 512)
	for i := range 3000 { // a heavy oldest day
		r := okReq(fmt.Sprintf("h%d", i), 1, ms(day0.AddDate(0, 0, -3))+int64(i))
		r.Path += pad
		s.Observe(r)
	}
	mustSync(t, s, false)
	allBytes, err := s.liveBytes()
	if err != nil {
		t.Fatal(err)
	}
	if allBytes < lightBytes+1<<20 {
		t.Fatalf("heavy day too small to test: %d -> %d", lightBytes, allBytes)
	}

	// A bound between the two: dropping the heavy oldest day is enough.
	s.SetLimits(0, lightBytes+(allBytes-lightBytes)/2)
	mustSync(t, s, true)
	if got, want := strings.Join(dayTables(t, s), ","), "20261012,20261011,20261010"; got != want {
		t.Fatalf("days = %s, want %s", got, want)
	}
	if _, bound := s.Limits(); func() bool { live, _ := s.liveBytes(); return live > bound }() {
		t.Fatalf("live still over %d", bound)
	}

	// A bound nothing fits: every day but today goes.
	s.SetLimits(0, 1)
	mustSync(t, s, true)
	if got := strings.Join(dayTables(t, s), ","); got != "20261012" {
		t.Fatalf("days = %s, want only today", got)
	}
	rows := allRequests(t, s, RequestQuery{DeploymentID: 1})
	if len(rows) != 20 {
		t.Fatalf("today kept %d rows, want 20", len(rows))
	}
	if st := s.Stats(); st.PartitionsDropped != 3 || st.Partitions != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if _, err := s.Coverage(context.Background(), 1, 0, 0); err != nil {
		t.Fatal(err)
	}
}
