package proxy

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// The in-flight table: every request the gateway is forwarding, so the
// monitor can show what each one is doing right now. A request enters it
// when it is handed to an instance and leaves when its reply ends; while in
// it, only its stream observer writes its timeline (atomics), and a reader
// copies one deployment's entries. Nothing here is persisted.

// liveRequest is one forwarded request in flight. A PD request enters the
// table when its prefill stage begins (stage set, not yet dispatched); the
// dispatch then fills in what the gateway learns on handing it to the
// decode instance.
type liveRequest struct {
	id           string
	traceID      tracecontext.TraceID
	deploymentID uint
	caller       uxrecord.Caller
	receivedAt   time.Time
	stage        *pdStage

	// mu guards what the dispatch sets; dispatchedAt is zero before it.
	mu           sync.Mutex
	instanceUUID string
	dispatchedAt time.Time
	stream       bool
	bodyBytes    int64
	images       int32

	// Nanoseconds since dispatchedAt; 0 means not yet.
	firstThink, firstAnswer, lastEvent atomic.Int64
	events, thinkEvents                atomic.Int32
}

// dispatch records the hand-off to the instance.
func (l *liveRequest) dispatch(at time.Time, instanceUUID string, body bodyFacts) {
	l.mu.Lock()
	l.dispatchedAt, l.instanceUUID = at, instanceUUID
	l.stream, l.bodyBytes, l.images = body.stream, body.bytes, body.images
	l.mu.Unlock()
}

// noteEvent records a text event of the request's stream. A nil request
// (an observer outside the table) ignores it.
func (l *liveRequest) noteEvent(at time.Duration, think, answer bool) {
	if l == nil {
		return
	}
	ns := max(int64(at), 1)
	if think {
		l.thinkEvents.Add(1)
		l.firstThink.CompareAndSwap(0, ns)
	}
	if answer {
		l.firstAnswer.CompareAndSwap(0, ns)
	}
	l.events.Add(1)
	l.lastEvent.Store(ns)
}

func (l *liveRequest) snapshot(now time.Time) uxrecord.Live {
	l.mu.Lock()
	dispatchedAt, instanceUUID := l.dispatchedAt, l.instanceUUID
	stream, bodyBytes, images := l.stream, l.bodyBytes, l.images
	l.mu.Unlock()
	out := uxrecord.Live{
		ID: l.id, TraceID: l.traceID, DeploymentID: l.deploymentID, InstanceUUID: instanceUUID,
		APIKeyID: l.caller.APIKeyID, UserID: l.caller.UserID, Source: l.caller.Source,
		ReceivedMS: l.receivedAt.UnixMilli(), AgeMS: now.Sub(l.receivedAt).Milliseconds(),
		Stream: stream, BodyBytes: bodyBytes, Images: images,
		DispatchMS: uxrecord.Unknown, FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: uxrecord.Unknown, LastEventMS: uxrecord.Unknown,
		Events: l.events.Load(), ThinkEvents: l.thinkEvents.Load(), PDPrefillStartMS: uxrecord.Unknown,
	}
	if l.stage != nil {
		out.PDPrefillInstance = l.stage.prefillInstance()
		out.PDPrefillStartMS = int32(l.stage.started.Sub(l.receivedAt) / time.Millisecond)
	}
	if dispatchedAt.IsZero() {
		return out
	}
	dispatch := dispatchedAt.Sub(l.receivedAt)
	rel := func(ns int64) int32 {
		if ns == 0 {
			return uxrecord.Unknown
		}
		return int32((dispatch + time.Duration(ns)) / time.Millisecond)
	}
	out.DispatchMS = int32(dispatch / time.Millisecond)
	out.FirstThinkMS, out.FirstAnswerMS, out.LastEventMS = rel(l.firstThink.Load()), rel(l.firstAnswer.Load()), rel(l.lastEvent.Load())
	return out
}

const liveShards = 64

// liveTable holds the in-flight requests, sharded by deployment so a busy
// deployment's requests never contend with another's.
type liveTable struct {
	shards [liveShards]struct {
		mu   sync.Mutex
		deps map[uint]map[*liveRequest]struct{}
	}
}

func (t *liveTable) shard(deploymentID uint) int { return int(deploymentID % liveShards) }

func (t *liveTable) add(l *liveRequest) {
	s := &t.shards[t.shard(l.deploymentID)]
	s.mu.Lock()
	if s.deps == nil {
		s.deps = map[uint]map[*liveRequest]struct{}{}
	}
	reqs := s.deps[l.deploymentID]
	if reqs == nil {
		reqs = map[*liveRequest]struct{}{}
		s.deps[l.deploymentID] = reqs
	}
	reqs[l] = struct{}{}
	s.mu.Unlock()
}

func (t *liveTable) remove(l *liveRequest) {
	s := &t.shards[t.shard(l.deploymentID)]
	s.mu.Lock()
	if reqs := s.deps[l.deploymentID]; reqs != nil {
		delete(reqs, l)
		if len(reqs) == 0 {
			delete(s.deps, l.deploymentID)
		}
	}
	s.mu.Unlock()
}

func (t *liveTable) snapshot(deploymentID uint, limit int, now time.Time) uxrecord.LiveSnapshot {
	s := &t.shards[t.shard(deploymentID)]
	s.mu.Lock()
	all := make([]uxrecord.Live, 0, len(s.deps[deploymentID]))
	for l := range s.deps[deploymentID] {
		all = append(all, l.snapshot(now))
	}
	s.mu.Unlock()
	var out uxrecord.LiveSnapshot
	rank := map[string]int{uxrecord.PhaseWaiting: 0, uxrecord.PhaseThinking: 1, uxrecord.PhaseAnswering: 2}
	for _, r := range all {
		switch r.Phase() {
		case uxrecord.PhaseWaiting:
			out.Waiting++
		case uxrecord.PhaseThinking:
			out.Thinking++
		default:
			out.Answering++
		}
	}
	sort.Slice(all, func(i, j int) bool {
		pi, pj := rank[all[i].Phase()], rank[all[j].Phase()]
		if pi != pj {
			return pi < pj
		}
		if all[i].ReceivedMS != all[j].ReceivedMS {
			return all[i].ReceivedMS < all[j].ReceivedMS
		}
		return all[i].ID < all[j].ID
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	out.Requests = all
	return out
}

// LiveRequests returns a deployment's in-flight snapshot.
func (p *Proxy) LiveRequests(deploymentID uint, limit int) uxrecord.LiveSnapshot {
	if p == nil {
		return uxrecord.LiveSnapshot{}
	}
	return p.live.snapshot(deploymentID, limit, time.Now())
}
