//go:build linux && dqlite

package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
)

func TestRetryStaleConnectionRepeatsOnlyADroppedConnection(t *testing.T) {
	stale := fmt.Errorf("close sessions: %w", driver.ErrBadConn)
	other := errors.New("constraint failed")
	cases := []struct {
		name      string
		failures  []error
		wantCalls int
		wantErr   error
	}{
		{"first attempt succeeds", nil, 1, nil},
		{"a dropped connection is retried", []error{stale, stale}, 3, nil},
		{"the fifth dropped connection is returned", []error{stale, stale, stale, stale, stale}, 5, driver.ErrBadConn},
		{"another error is returned at once", []error{other}, 1, other},
		{"another error after a retry is returned", []error{stale, other}, 2, other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := retryStaleConnection(context.Background(), func() error {
				calls++
				if calls <= len(tc.failures) {
					return tc.failures[calls-1]
				}
				return nil
			})
			if calls != tc.wantCalls || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("calls = %d, err = %v; want %d calls and %v", calls, err, tc.wantCalls, tc.wantErr)
			}
		})
	}
}

func TestRetryStaleConnectionStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := retryStaleConnection(ctx, func() error {
		calls++
		return driver.ErrBadConn
	})
	if calls != 1 || !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("calls = %d, err = %v; want one attempt and the connection error", calls, err)
	}
}
