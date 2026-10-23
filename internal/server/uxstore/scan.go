package uxstore

import (
	"context"
	"fmt"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// RequestDay is a UTC day the request log holds rows for.
type RequestDay struct {
	Name    string // YYYYMMDD
	StartMS int64
}

// RequestDays lists the days with request rows, oldest first.
func (s *Store) RequestDays() ([]RequestDay, error) {
	days, err := listDays(s.reader)
	if err != nil {
		return nil, err
	}
	slices.Sort(days)
	out := make([]RequestDay, 0, len(days))
	for _, d := range days {
		start, err := dayStartMS(d)
		if err != nil {
			return nil, err
		}
		out = append(out, RequestDay{Name: d, StartMS: start})
	}
	return out, nil
}

// scanPage bounds one read of ScanDay, so it never holds a reader long.
const scanPage = 2000

// ScanDay hands fn every request row of day received before beforeMS, in the
// order they were written, a page at a time. Each row's Weight is how many
// requests it stands for. A day without a request table scans nothing.
func (s *Store) ScanDay(ctx context.Context, day string, beforeMS int64, fn func(uxrecord.Request) error) error {
	table := reqPrefix + day
	var exists int
	if err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	after := int64(-1)
	for {
		rows, err := s.reader.QueryContext(ctx, `SELECT `+reqColumns+` FROM `+table+` r WHERE r.seq > ? AND r.ts < ? ORDER BY r.seq LIMIT ?`,
			after, beforeMS, scanPage)
		if err != nil {
			return fmt.Errorf("scan %s: %w", table, err)
		}
		n := 0
		for rows.Next() {
			var seq int64
			r, err := scanRequest(rows, &seq)
			if err != nil {
				rows.Close()
				return err
			}
			after, n = seq, n+1
			if err := fn(r); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if n < scanPage {
			return nil
		}
	}
}
