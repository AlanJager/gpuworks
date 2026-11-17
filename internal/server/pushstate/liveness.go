package pushstate

import (
	"context"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
)

// OwnerUp records that owner's process generation is running and
// answering: its grants stop counting toward the grace, which a later
// OwnerDown starts afresh. A push over the connection of an older
// generation is refused from now on.
func (s *State) OwnerUp(owner string, generation uint64) {
	s.mu.Lock()
	if generation > s.spawn[owner] {
		s.spawn[owner] = generation
	}
	if _, down := s.down[owner]; !down {
		s.mu.Unlock()
		return
	}
	delete(s.down, owner)
	s.downDirty[owner] = true
	s.changed()
}

// OwnerDown records that owner is down (crashed, crash looping, disabled,
// stopped) from now on, unless it already was: the earliest start is kept
// and persisted, so neither the core's restart nor the plugin's restarts
// renew the grace. What it restricted stays in force; its grants narrow
// once the grace has passed.
func (s *State) OwnerDown(owner string) {
	s.mu.Lock()
	if _, down := s.down[owner]; down {
		s.mu.Unlock()
		return
	}
	s.down[owner] = s.clock.nowMS()
	s.downDirty[owner] = true
	s.changed()
}

// changed recombines after an in-memory change made under the lock,
// releases it, wakes the writer tick for what it must persist and runs the
// effects.
func (s *State) changed() {
	holds, features := s.recombineLocked()
	s.mu.Unlock()
	s.poke()
	s.effects(holds, features)
}

// Uninstall removes everything owner pushed, its overrides and its down
// record, in one commit, and releases its holds in one pass of the
// scheduling holds. Running work is never touched: a released node only
// becomes schedulable again. The caller has already dropped the package
// from SetPackages, so it can push nothing more.
func (s *State) Uninstall(ctx context.Context, owner string) error {
	return s.enqueue(ctx, &op{
		build: func(b *pstate.Batch) { b.Uninstall = append(b.Uninstall, owner) },
		after: func(_ pstate.Result, err error) {
			if err != nil {
				return
			}
			for k := range s.parts {
				if k.owner == owner {
					delete(s.parts, k)
				}
			}
			for id, o := range s.overrides {
				if o.Owner == owner {
					delete(s.overrides, id)
				}
			}
			delete(s.down, owner)
			delete(s.downDirty, owner)
			delete(s.spawn, owner)
		},
	})
}

// CorrectClock moves extclock to to, the correction of a clock that ran
// ahead: by the root-run offline reset, or by the package holding the
// clock_authority role relaying a correction it verified. One correction
// moves it at most MaxClockCorrection either way. Steps are re-evaluated at
// the corrected time and are not consumed, so moving back un-fires the steps
// after to. by names who corrected it, for the audit and the banner.
func (s *State) CorrectClock(ctx context.Context, to time.Time, by string) error {
	toMS := to.UnixMilli()
	if d := time.Duration(toMS-s.clock.nowMS()) * time.Millisecond; d > MaxClockCorrection || d < -MaxClockCorrection {
		return messages.NewProductError(messages.ErrorPluginClockCorrectionTooLarge,
			fmt.Sprintf("a correction moves extclock by at most %d days", int(MaxClockCorrection/(24*time.Hour))),
			map[string]any{"max_days": int(MaxClockCorrection / (24 * time.Hour))})
	}
	var fromMS int64
	err := s.enqueue(ctx, &op{
		build: func(b *pstate.Batch) {
			fromMS = s.clock.correct(toMS)
			b.Clock = &pstate.Clock{HWMMS: toMS}
		},
		after: func(_ pstate.Result, err error) {
			if err != nil {
				s.hwmDirty = true
				return
			}
			s.corrected = &clockCorrection{fromMS: fromMS, toMS: toMS, by: by}
		},
	})
	if err != nil {
		return err
	}
	if f := s.cfg.Effects.ClockCorrected; f != nil {
		f(time.UnixMilli(fromMS).UTC(), time.UnixMilli(toMS).UTC(), by)
	}
	return nil
}
