package uxstore

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func TestScanDayPagesThroughADayBeforeACutoff(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.RowsPerSecond = 10 * scanPage // the clock stands still: keep every row
	s, _ := openTest(t, opts)
	for i := 0; i < scanPage+5; i++ {
		s.Observe(okReq(fmt.Sprint("a", i), 1, ms(day0)+int64(i)*1000))
	}
	s.Observe(okReq("yesterday", 2, ms(day0.AddDate(0, 0, -1))))
	mustSync(t, s, false)
	days, err := s.RequestDays()
	if err != nil || len(days) != 2 || days[0].Name != "20261011" || days[1].Name != "20261012" || days[1].StartMS-days[0].StartMS != dayMS {
		t.Fatalf("days = %+v %v", days, err)
	}
	var n int
	var weights int64
	err = s.ScanDay(context.Background(), "20261012", ms(day0)+int64(scanPage+2)*1000, func(r uxrecord.Request) error {
		n++
		weights += int64(r.Weight)
		return nil
	})
	if err != nil || n != scanPage+2 || weights != int64(scanPage+2) {
		t.Fatalf("scanned %d (weights %d) %v", n, weights, err)
	}
	stop := errors.New("stop")
	if err := s.ScanDay(context.Background(), "20261012", ms(day0)+1e9, func(uxrecord.Request) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error = %v", err)
	}
	if err := s.ScanDay(context.Background(), "20200101", 1e15, func(uxrecord.Request) error { t.Fatal("row from a missing day"); return nil }); err != nil {
		t.Fatal(err)
	}
}
