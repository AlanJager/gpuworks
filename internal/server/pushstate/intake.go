package pushstate

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// pendingPush is a validated push waiting for the writer tick.
type pendingPush struct {
	part *part
	done chan pushResult
}

type pushResult struct {
	ack *extapi.StateAck
	err *extapi.WireError
}

func wireError(code, reason string, params map[string]any, format string, args ...any) *extapi.WireError {
	return &extapi.WireError{Code: code, Message: fmt.Sprintf(format, args...), Params: extapi.Params(params), Reason: reason}
}

func wireOf(err error, reason string) *extapi.WireError {
	if e, ok := err.(*extapi.Error); ok {
		if e.Code == extapi.ErrorSchemaTooNew {
			reason = extapi.ReasonUnsupported
		}
		w := extapi.WireErrorOf(e, reason)
		return &w
	}
	return wireError(extapi.ErrorWireInvalid, reason, nil, "%v", err)
}

// Push takes one push from pusher: checks it, queues it, and returns once
// the writer tick committed it (an applied ack), a newer push of the same
// owner and kind replaced it before the commit (a superseded ack), or it was
// refused (a gwx/1 error body). The owner is pusher, never anything in the
// push. Push waits for the commit, at most about a tick; ctx bounds the
// wait, and a push whose wait ended may still commit.
func (s *State) Push(ctx context.Context, pusher Pusher, push *extapi.StatePush) (*extapi.StateAck, *extapi.WireError) {
	if push == nil {
		return nil, wireError(extapi.ErrorWireInvalid, extapi.ReasonInvalidArgument, map[string]any{"object": "state_push", "field": "body", "rule": extapi.RuleRequired}, "no push")
	}
	if len(push.Body) > extapi.MaxPushBytes {
		return nil, wireError(extapi.ErrorStateTooLarge, extapi.ReasonInvalidArgument, map[string]any{"kind": push.Kind, "field": "body", "max": extapi.MaxPushBytes}, "pushed %s exceeds %d bytes", push.Kind, extapi.MaxPushBytes)
	}
	if err := push.Validate(); err != nil {
		return nil, wireOf(err, extapi.ReasonInvalidArgument)
	}
	if w := permitted(&pusher, push.Kind); w != nil {
		return nil, w
	}
	table, err := push.DecodeBody()
	if err != nil {
		return nil, wireOf(err, extapi.ReasonInvalidArgument)
	}
	digest, err := extapi.DigestValue(table)
	if err != nil {
		return nil, wireOf(err, extapi.ReasonInvalidArgument)
	}
	p := &part{owner: pusher.ID, kind: push.Kind, schema: push.Schema, gen: push.Generation, digest: digest, table: normalize(table)}

	s.mu.Lock()
	if w := s.ownsLocked(&pusher, p.table); w != nil {
		s.mu.Unlock()
		return nil, w
	}
	if w := s.freshLocked(&pusher, p); w != nil {
		s.mu.Unlock()
		return nil, w
	}
	p.pushedAt = s.clock.nowMS()
	compile(p)
	k := ownerKind{p.owner, p.kind}
	pp := &pendingPush{part: p, done: make(chan pushResult, 1)}
	if old := s.pending[k]; old != nil {
		old.done <- pushResult{ack: &extapi.StateAck{Kind: p.kind, Generation: old.part.gen, SupersededBy: p.gen}}
	}
	s.pending[k] = pp
	s.mu.Unlock()
	s.poke()

	select {
	case r := <-pp.done:
		return r.ack, r.err
	case <-ctx.Done():
		return nil, wireError(messages.ErrorPluginStateNotSaved, extapi.ReasonCancelled, map[string]any{"kind": p.kind},
			"the wait for the %s push to commit ended: %v", p.kind, ctx.Err())
	}
}

// poke wakes the writer tick.
func (s *State) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// permitted checks the pusher may push kind at all: availability is
// checked row by row against the namespace authority, fleet capacity needs
// the certificate's privileged table grant, attestations the namespace
// authority role, and the rest the approved push:<kind> permission.
func permitted(p *Pusher, kind string) *extapi.WireError {
	var ok bool
	needs := "push:" + kind
	switch kind {
	case extapi.KindAvailability:
		return nil
	case extapi.KindFleetCapacity:
		ok, needs = slices.Contains(p.Grants.Tables, kind), "grants.tables:"+kind
	case extapi.KindAttestations:
		ok, needs = p.Grants.AuthorityID != "" && p.Grants.AuthorityID == p.ID, "grants.authority_id"
	case extapi.KindNodeHolds, extapi.KindBanners:
		ok = p.hasPermission(needs)
	default:
		// A kind this build does not define: decoding answers it.
		return nil
	}
	if ok {
		return nil
	}
	return wireError(messages.ErrorPluginStateNotPermitted, extapi.ReasonPermissionDenied, map[string]any{"kind": kind, "needs": needs},
		"plugin %s may not push %s: it needs %s", p.ID, kind, needs)
}

func notOwner(kind, field, row, format string, args ...any) *extapi.WireError {
	return wireError(messages.ErrorPluginStateNotOwner, extapi.ReasonPermissionDenied, map[string]any{"kind": kind, "field": field, "row": row}, format, args...)
}

// ownsLocked checks every row names only what the pusher answers for:
// feature keys it is the authority for (or declared with gate:self), and
// codes its manifest lists.
func (s *State) ownsLocked(p *Pusher, t extapi.Table) *extapi.WireError {
	if installed, _ := s.reg.installed(p.ID); !installed {
		return notOwner(t.TableKind(), "", "", "package %s is not installed", p.ID)
	}
	kind := t.TableKind()
	code := func(field, row string, n *extapi.Notice) *extapi.WireError {
		if n == nil || p.mayUseCode(n.Code) {
			return nil
		}
		return notOwner(kind, field, row, "code %s is not one package %s lists", n.Code, p.ID)
	}
	key := func(field, row, k string) *extapi.WireError {
		if k == "" || s.reg.answers(p.ID, p.Publisher, featuregate.Key(k)) {
			return nil
		}
		return notOwner(kind, field, row, "package %s does not answer for feature %s", p.ID, k)
	}
	switch t := t.(type) {
	case *extapi.AvailabilityV1:
		for i, r := range t.Rows {
			at := "rows." + strconv.Itoa(i)
			if w := key(at+".feature", r.Feature, r.Feature); w != nil {
				return w
			}
			for j, st := range r.Steps {
				if w := code(at+".steps."+strconv.Itoa(j)+".notice.code", r.Feature, st.Notice); w != nil {
					return w
				}
			}
		}
	case *extapi.FleetCapacityV1:
		for _, f := range []struct {
			field  string
			notice *extapi.Notice
		}{{"refuse_node", &t.RefuseNode}, {"refuse_card", &t.RefuseCard}, {"hold", &t.Hold}} {
			if w := code(f.field+".code", "", f.notice); w != nil {
				return w
			}
		}
	case *extapi.NodeHoldsV1:
		ids := make([]string, 0, len(t.Nodes))
		for id := range t.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			n := t.Nodes[id]
			if w := code("nodes."+id+".code", id, &n); w != nil {
				return w
			}
		}
	case *extapi.BannersV1:
		for i, b := range t.Banners {
			for j, st := range b.Steps {
				if w := code("banners."+strconv.Itoa(i)+".steps."+strconv.Itoa(j)+".notice.code", b.ID, st.Notice); w != nil {
					return w
				}
			}
		}
	case *extapi.AttestationsV1:
		for i, a := range t.Attestations {
			if w := key("attestations."+strconv.Itoa(i)+".feature", a.Name, a.Feature); w != nil {
				return w
			}
		}
	}
	return nil
}

// freshLocked checks the push is newer than what the core holds or is about
// to commit, from the owner's current process.
func (s *State) freshLocked(p *Pusher, next *part) *extapi.WireError {
	if p.Generation < s.spawn[p.ID] {
		return wireError(messages.ErrorPluginStateStale, extapi.ReasonFailedPrecondition,
			map[string]any{"kind": next.kind, "generation": int64(p.Generation), "held": int64(s.spawn[p.ID])},
			"plugin %s pushed over the connection of process %d, replaced by %d", p.ID, p.Generation, s.spawn[p.ID])
	}
	s.spawn[p.ID] = p.Generation
	k := ownerKind{next.owner, next.kind}
	var held uint64
	if c := s.parts[k]; c != nil {
		held = c.gen
	}
	if q := s.pending[k]; q != nil && q.part.gen > held {
		held = q.part.gen
	}
	if g := s.inflight[k]; g > held {
		held = g
	}
	if next.gen <= held {
		return wireError(messages.ErrorPluginStateStale, extapi.ReasonFailedPrecondition,
			map[string]any{"kind": next.kind, "generation": int64(next.gen), "held": int64(held)},
			"plugin %s pushed %s at generation %d; the server holds %d", p.ID, next.kind, next.gen, held)
	}
	return nil
}
