//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Leadership moves while every member writes, guarded material records among
// the writes. A write may fail then, as it could before the check existed;
// what must not happen is that the failure reads as "this member is not
// admitted": no error is a refusal, nothing reports one, and every member
// writes again once a leader is back.
func TestNativeLeaderChangeDuringGuardedWritesIsNotARefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	expected := f.expected()
	key := store.SettingKey("leader-change-guard")
	var refused, failed, written atomic.Int64
	var first atomic.Pointer[error]
	for _, st := range f.stores {
		st.OnWriterRefused(func() { refused.Add(1) })
	}
	var stop atomic.Bool
	var writers sync.WaitGroup
	for index, st := range f.stores {
		writers.Go(func() {
			for n := 0; !stop.Load(); n++ {
				attempt, end := context.WithTimeout(ctx, 3*time.Second)
				var err error
				if n%2 == 0 {
					err = st.SetBusinessSetting(attempt, key, `"v"`)
				} else {
					// A guarded cluster record: this member's enrollment origin.
					err = st.SetClusterEnrollmentOriginForVerifiedKey(attempt, expected, f.keys[index], "https://127.0.0.1:1")
				}
				end()
				switch {
				case err == nil:
					written.Add(1)
				case errors.Is(err, store.ErrClusterAdmissionRejected):
					t.Errorf("member %d: a write during a leadership change was refused as not admitted: %v", index, err)
					return
				default:
					failed.Add(1)
					first.CompareAndSwap(nil, &err)
				}
			}
		})
	}
	moves := 0
	for range 6 {
		time.Sleep(700 * time.Millisecond)
		leader, err := f.databases[0].Leader(ctx)
		if err != nil {
			continue
		}
		for _, db := range f.databases {
			if db.ID != leader.ID {
				move, end := context.WithTimeout(ctx, 5*time.Second)
				if err := f.databases[0].Transfer(move, db.ID); err == nil {
					moves++
				}
				end()
				break
			}
		}
	}
	stop.Store(true)
	writers.Wait()
	if moves == 0 {
		t.Fatal("leadership never moved")
	}
	time.Sleep(100 * time.Millisecond)
	if refused.Load() != 0 {
		t.Fatalf("%d refusals were reported during %d leadership changes", refused.Load(), moves)
	}
	for index, st := range f.stores {
		var err error
		for range 50 {
			if err = st.SetBusinessSetting(ctx, key, `"after"`); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Errorf("member %d cannot write after the leadership changes: %v", index, err)
		}
	}
	firstErr := error(nil)
	if p := first.Load(); p != nil {
		firstErr = *p
	}
	t.Logf("%d leadership changes; %d writes committed, %d failed without being a refusal (first: %v)", moves, written.Load(), failed.Load(), firstErr)
}
