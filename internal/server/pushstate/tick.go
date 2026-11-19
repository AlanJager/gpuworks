package pushstate

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// op is a change other than a push that the writer tick commits: build
// checks it against the state of the tick and adds its rows to the batch,
// or refuses it and adds nothing; after applies it in memory once the batch
// committed (err nil) or reports the failure, and is not called for a
// refused op. Both run under the lock.
type op struct {
	build   func(b *pstate.Batch) error
	after   func(res pstate.Result, err error)
	done    chan error
	refused error
}

// enqueue queues o for the next tick and waits for its commit. A ctx done
// before a tick took o withdraws it; once a tick took it, enqueue waits for
// that tick's answer, so the caller never reports a change as refused that
// then commits.
func (s *State) enqueue(ctx context.Context, o *op) error {
	o.done = make(chan error, 1)
	s.mu.Lock()
	s.ops = append(s.ops, o)
	s.mu.Unlock()
	s.poke()
	select {
	case err := <-o.done:
		return err
	case <-ctx.Done():
	}
	s.mu.Lock()
	if i := slices.Index(s.ops, o); i >= 0 {
		s.ops = slices.Delete(s.ops, i, i+1)
		s.mu.Unlock()
		return ctx.Err()
	}
	s.mu.Unlock()
	return <-o.done
}

// Run runs the writer tick, the step timer and the capacity selection until
// ctx is done, then persists extclock's high-water mark. Every change of one
// tick, from every owner, commits in one writer transaction, and ticks are
// at least TickInterval apart.
func (s *State) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	if s.cfg.Bus != nil {
		// Subscribe before reading the fleet again, so no change between
		// the two is missed.
		sub := s.cfg.Bus.Subscribe(64, s.cfg.FleetTopic)
		defer sub.Close()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-sub.C():
					s.fleetDirty.Store(true)
					s.sel.Request()
				}
			}
		}()
	}
	s.fleetDirty.Store(true)
	s.sel.Request()
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.sel.Run(ctx)
	}()
	s.tickLoop(ctx)
	wg.Wait()
	return nil
}

func (s *State) tickLoop(ctx context.Context) {
	var last time.Time
	hwm := time.NewTicker(hwmPersistEvery)
	defer hwm.Stop()
	for {
		s.mu.Lock()
		wait := timerWait(s.next, s.clock.nowMS())
		s.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.shutdown()
			return
		case <-s.wake:
			if d := s.cfg.TickInterval - time.Since(last); !last.IsZero() && d > 0 {
				select {
				case <-ctx.Done():
					timer.Stop()
					s.shutdown()
					return
				case <-time.After(d):
				}
			}
			last = time.Now()
			s.commit(ctx)
		case <-timer.C:
			s.mu.Lock()
			holds, features := s.recombineLocked()
			s.mu.Unlock()
			s.effects(holds, false, features)
		case <-hwm.C:
			s.mu.Lock()
			s.hwmDirty = true
			s.mu.Unlock()
			s.commit(ctx)
		}
		timer.Stop()
	}
}

// timerWait is how long the step timer sleeps before the step due at next
// (extclock milliseconds; math.MaxInt64 when none is due), capped at
// maxTimerWait. The difference is clamped in milliseconds before it becomes
// a Duration: next-now for "no step" is ~9.2e18 ms, and converting that to
// nanoseconds overflows, which once made an idle server spin on a zero
// timer.
func timerWait(next, now int64) time.Duration {
	ms := next - now
	if ms <= 0 {
		return 0
	}
	if ms >= maxTimerWait.Milliseconds() {
		return maxTimerWait
	}
	return time.Duration(ms) * time.Millisecond
}

// shutdown commits what is pending, the high-water mark included, and
// refuses whatever could not be committed.
func (s *State) shutdown() {
	s.mu.Lock()
	s.hwmDirty = true
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.commit(ctx)
	s.mu.Lock()
	pending, ops := s.pending, s.ops
	s.pending, s.ops = map[ownerKind]*pendingPush{}, nil
	s.mu.Unlock()
	for _, pp := range pending {
		pp.done <- notSaved(pp.part.kind)
	}
	for _, o := range ops {
		o.done <- context.Canceled
	}
}

func notSaved(kind string) pushResult {
	w := wireError(messages.ErrorPluginStateNotSaved, extapi.ReasonUnavailable, map[string]any{"kind": kind},
		"the server could not save the %s push", kind)
	w.RetryAfterMS = 1000
	return pushResult{err: w}
}

// tickWork is what one tick took out of the queues.
type tickWork struct {
	pushes  []*pendingPush
	ops     []*op
	down    map[string]bool
	hwm     bool
	dropped []Override
}

// commit runs one writer tick: it takes every pending change, writes them
// in one transaction, then applies them in memory, recombines and answers
// the waiters. Nothing is applied when the write fails.
func (s *State) commit(ctx context.Context) {
	s.mu.Lock()
	b, w := s.buildLocked()
	s.mu.Unlock()
	if b.Empty() && len(w.ops) == 0 {
		return
	}
	res, err := s.cfg.Store.WritePushedState(ctx, b)

	s.mu.Lock()
	results := s.applyLocked(b, w, res, err)
	s.holdsDirty = s.holdsDirty || err == nil
	holds, features := s.recombineLocked()
	s.mu.Unlock()
	// Effects first, so a waiter that returns sees everything its commit
	// caused.
	if err == nil {
		for _, o := range w.dropped {
			if f := s.cfg.Effects.OverrideSuperseded; f != nil {
				f(o)
			}
		}
	}
	s.effects(holds, false, features)
	for pp, r := range results {
		// Counted here, not by the waiter: a push is committed whether
		// or not its caller still waits for the ack.
		if f := s.cfg.Effects.Applied; f != nil && r.ack != nil && r.ack.AppliedAt != nil {
			f(pp.part.owner, pp.part.kind, EntriesOf(pp.part.table))
		}
		pp.done <- r
	}
	for _, o := range w.ops {
		if o.refused != nil {
			o.done <- o.refused
			continue
		}
		o.done <- err
	}
}

func (s *State) buildLocked() (*pstate.Batch, *tickWork) {
	b := &pstate.Batch{}
	w := &tickWork{down: s.downDirty, hwm: s.hwmDirty}
	s.downDirty, s.hwmDirty = map[string]bool{}, false
	for _, o := range s.ops {
		o.refused = o.build(b)
	}
	w.ops, s.ops = s.ops, nil
	// An override an op of this tick already drops (withdrawn or replaced)
	// is not also superseded by a push of the same tick.
	claimed := map[int64]bool{}
	for _, id := range b.DropOverrides {
		claimed[id] = true
	}
	uninstalled := map[string]bool{}
	for _, id := range b.Uninstall {
		uninstalled[id] = true
	}
	for k, pp := range s.pending {
		delete(s.pending, k)
		if uninstalled[k.owner] {
			pp.done <- pushResult{err: notOwner(k.kind, "", "", "package %s was uninstalled", k.owner)}
			continue
		}
		old := s.parts[k]
		b.Tables = append(b.Tables, tableWrite(old, pp.part))
		w.pushes = append(w.pushes, pp)
		s.inflight[k] = pp.part.gen
		for _, o := range s.overrides {
			if !claimed[o.ID] && o.Owner == k.owner && o.Kind == k.kind && old != nil &&
				!reflect.DeepEqual(entryValue(old.table, o.Entry), entryValue(pp.part.table, o.Entry)) {
				b.DropOverrides = append(b.DropOverrides, o.ID)
				w.dropped = append(w.dropped, *o)
			}
		}
	}
	for owner := range w.down {
		if since, down := s.down[owner]; down {
			b.Down = append(b.Down, pstate.Down{Owner: owner, SinceMS: since})
		} else {
			b.Up = append(b.Up, owner)
		}
	}
	if w.hwm && b.Clock == nil {
		b.Clock = &pstate.Clock{HWMMS: s.clock.nowMS()}
	}
	b.Markers, s.markers = s.markers, nil
	return b, w
}

func (s *State) applyLocked(b *pstate.Batch, w *tickWork, res pstate.Result, err error) map[*pendingPush]pushResult {
	results := make(map[*pendingPush]pushResult, len(w.pushes))
	for _, pp := range w.pushes {
		delete(s.inflight, ownerKind{pp.part.owner, pp.part.kind})
	}
	for _, o := range w.ops {
		if o.refused == nil {
			o.after(res, err)
		}
	}
	if err != nil {
		s.log.Error("commit pushed state", "err", err, "pushes", len(w.pushes))
		for owner := range w.down {
			s.downDirty[owner] = true
		}
		s.hwmDirty = s.hwmDirty || b.Clock != nil
		s.markers = append(s.markers, b.Markers...)
		for _, pp := range w.pushes {
			results[pp] = notSaved(pp.part.kind)
		}
		return results
	}
	if b.Clock != nil {
		s.hwmSaved = b.Clock.HWMMS
	}
	stale := map[ownerKind]bool{}
	for _, k := range res.Stale {
		stale[ownerKind{k.Owner, k.Kind}] = true
	}
	for _, id := range b.DropOverrides {
		delete(s.overrides, id)
	}
	for _, o := range w.dropped {
		s.superseded[o.ID] = supersededOverride{o: o, atMS: s.clock.nowMS()}
	}
	appliedAt := extapi.NewTime(time.UnixMilli(s.clock.nowMS()))
	for _, pp := range w.pushes {
		p := pp.part
		k := ownerKind{p.owner, p.kind}
		if stale[k] {
			results[pp] = pushResult{err: wireError(messages.ErrorPluginStateStale, extapi.ReasonFailedPrecondition,
				map[string]any{"kind": p.kind, "generation": int64(p.gen), "held": int64(s.heldGen(k))}, "a newer %s push is stored", p.kind)}
			continue
		}
		s.parts[k] = p
		at := appliedAt
		results[pp] = pushResult{ack: &extapi.StateAck{Kind: p.kind, Generation: p.gen, Digest: p.digest, AppliedAt: &at}}
	}
	return results
}

func (s *State) heldGen(k ownerKind) uint64 {
	if p := s.parts[k]; p != nil {
		return p.gen
	}
	return 0
}

// entryValue is the value of one override entry in a restriction table:
// what a new push must change for the override to be superseded.
func entryValue(t extapi.Table, entry string) any {
	switch t := t.(type) {
	case *extapi.NodeHoldsV1:
		n, ok := t.Nodes[entry]
		if !ok {
			return nil
		}
		return n
	case *extapi.FleetCapacityV1:
		type point struct {
			at  int64
			max *int64
		}
		var out []point
		for _, st := range t.Steps {
			v := st.MaxCards
			if entry == EntryMaxNodesWithCards {
				v = st.MaxNodesWithCards
			}
			out = append(out, point{timeMS(st.At), v})
		}
		return out
	}
	return nil
}
