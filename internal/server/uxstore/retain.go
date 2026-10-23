package uxstore

import (
	"fmt"
	"os"
	"time"
)

// SetLimits changes the age and size bounds (Options.MaxAge, MaxBytes) of a
// running store; the next retention pass, within a minute, applies them.
// A non-positive value keeps the current bound.
func (s *Store) SetLimits(maxAge time.Duration, maxBytes int64) {
	if maxAge > 0 {
		s.maxAge.Store(int64(maxAge))
	}
	if maxBytes > 0 {
		s.maxBytes.Store(maxBytes)
	}
}

// Limits are the bounds in effect.
func (s *Store) Limits() (time.Duration, int64) {
	return time.Duration(s.maxAge.Load()), s.maxBytes.Load()
}

// SizeBytes is the file's size on disk, its write-ahead log included. The
// file keeps its high water mark: dropped days' pages are reused, not freed.
func (s *Store) SizeBytes() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

// EarliestDayMS is the start of the oldest day the store holds, or false
// when it holds none.
func (s *Store) EarliestDayMS() (int64, bool, error) {
	days, err := listDays(s.reader)
	if err != nil || len(days) == 0 {
		return 0, false, err
	}
	oldest := days[0]
	for _, d := range days {
		if d < oldest {
			oldest = d
		}
	}
	start, err := dayStartMS(oldest)
	return start, err == nil, err
}

// maybeRetain runs retention at most once per retainEvery of the store clock.
func (s *Store) maybeRetain() {
	now := s.opts.Now()
	if now.Sub(s.w.lastRetain) < retainEvery {
		return
	}
	s.w.lastRetain = now
	if err := s.retain(); err != nil {
		s.retainErrors.Add(1)
	}
}

// retain drops whole days, on the writer goroutine (so nothing is pending for
// a day it drops; a late record for a dropped day recreates its tables):
//
//  1. Every day that ended at least MaxAge ago. Rows are never deleted one by
//     one, so a store keeps up to one day more than MaxAge.
//  2. Then, oldest first, days before today while the live size exceeds
//     MaxBytes. Today is never dropped, so a single day's traffic can exceed
//     the bound.
//
// The live size is the pages in use. Dropped tables' pages go to SQLite's
// free list and later writes reuse them, so the file stays near its high
// water mark instead of shrinking; the bound holds for what new data can
// grow into.
func (s *Store) retain() error {
	now := s.opts.Now().UnixMilli()
	today := dayOf(now)
	days, err := listDays(s.writer)
	if err != nil {
		return fmt.Errorf("list partitions: %w", err)
	}
	cutoff := now - time.Duration(s.maxAge.Load()).Milliseconds()
	maxBytes := s.maxBytes.Load()
	var kept []string // newest first
	for _, day := range days {
		start, err := dayStartMS(day)
		if err != nil {
			return err
		}
		if day < today && start+dayMS <= cutoff {
			if err := s.drop(day); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, day)
	}
	for i := len(kept) - 1; i >= 0 && kept[i] < today; i-- {
		live, err := s.liveBytes()
		if err != nil {
			return err
		}
		if live <= maxBytes {
			break
		}
		if err := s.drop(kept[i]); err != nil {
			return err
		}
	}
	// Advance the WAL without waiting on readers, so a long read snapshot
	// cannot let it grow unchecked.
	_, err = s.writer.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)
	return err
}

func (s *Store) drop(day string) error {
	if err := dropPartition(s.writer, day); err != nil {
		return err
	}
	if _, ok := s.w.ready[day]; ok {
		delete(s.w.ready, day)
		s.partitions.Add(-1)
	}
	s.partsDropped.Add(1)
	return nil
}

// liveBytes is the size of the pages in use (free-list pages excluded).
func (s *Store) liveBytes() (int64, error) {
	var pages, free, size int64
	if err := s.writer.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.writer.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, err
	}
	if err := s.writer.QueryRow(`PRAGMA page_size`).Scan(&size); err != nil {
		return 0, err
	}
	return (pages - free) * size, nil
}
