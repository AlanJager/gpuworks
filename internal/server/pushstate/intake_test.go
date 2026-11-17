package pushstate

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

const backupFull = `{"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full","limits":[{"dim":"gpus","max":4}]},{"access":"read_only","at":"2027-02-01T00:00:00Z","notice":{"code":"ACME_EXPIRED","params":{"days":0}}}]}]}`

func TestAnAuthorityPushIsCommittedAndEnforced(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	if got := h.s.Availability("acme.backup"); got.Access != featuregate.Locked || got.Notice.Code() != messages.ErrorFeatureProviderAbsent {
		t.Fatalf("before any push: %+v", got)
	}
	ack := h.push(pusherOf(acmeAuthority), extapi.KindAvailability, 1, backupFull)
	table, _ := extapi.DecodeTable(extapi.KindAvailability, 1, []byte(backupFull))
	want, _ := extapi.DigestValue(table)
	if ack.Generation != 1 || ack.Digest != want || ack.AppliedAt == nil || ack.Validate() != nil {
		t.Fatalf("ack = %+v (want digest %s)", ack, want)
	}
	got := h.s.Availability("acme.backup")
	if got.Access != featuregate.Full || len(got.Limits) != 1 || got.Limits[0] != (featuregate.Limit{Dim: "gpus", Max: 4}) {
		t.Fatalf("after push: %+v", got)
	}
	if h.s.Availability("acme.free").Access != featuregate.Full {
		t.Fatal("a gate:none key is always full")
	}
	if a := h.s.Availability("acme.reports"); a.Access != featuregate.Locked || a.Notice.Params()["feature"] != "acme.reports" {
		t.Fatalf("a declared key nobody pushed: %+v", a)
	}
	if a := h.s.Availability("nobody.declares"); a.Access != featuregate.Locked || a.Notice.Code() != messages.ErrorFeatureProviderAbsent {
		t.Fatalf("an undeclared key: %+v", a)
	}
	held := h.s.Held("acme_authority")
	if len(held) != 1 || held[0].Generation != 1 || held[0].Digest != want || held[0].Kind != extapi.KindAvailability {
		t.Fatalf("held = %+v", held)
	}
	if h.features.Load() == 0 {
		t.Fatal("no feature change was announced")
	}
}

func TestPushRefusals(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	authority := pusherOf(acmeAuthority, "push:banners")
	h.push(authority, extapi.KindAvailability, 5, backupFull)

	manyHolds := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"nodes":{`)
		for i := 1; i <= n; i++ {
			if i > 1 {
				b.WriteByte(',')
			}
			b.WriteString(`"` + strconv.Itoa(i) + `":{"code":"ACME_MAINTENANCE"}`)
		}
		b.WriteString(`}}`)
		return b.String()
	}
	cases := []struct {
		name   string
		pusher Pusher
		kind   string
		gen    uint64
		body   string
		code   string
		reason string
	}{
		{"same generation is stale", authority, extapi.KindAvailability, 5, backupFull, messages.ErrorPluginStateStale, extapi.ReasonFailedPrecondition},
		{"older generation is stale", authority, extapi.KindAvailability, 4, backupFull, messages.ErrorPluginStateStale, extapi.ReasonFailedPrecondition},
		{"a package that is not the authority", pusherOf(acmeBackup), extapi.KindAvailability, 1, backupFull, messages.ErrorPluginStateNotOwner, extapi.ReasonPermissionDenied},
		{"another publisher's authority", pusherOf(rogueAuthority), extapi.KindAvailability, 1, backupFull, messages.ErrorPluginStateNotOwner, extapi.ReasonPermissionDenied},
		{"a third party for a bare key", pusherOf(rogueAuthority), extapi.KindAvailability, 1,
			`{"rows":[{"down_access":"locked","feature":"audit","steps":[{"access":"full"}]}]}`, messages.ErrorPluginStateNotOwner, extapi.ReasonPermissionDenied},
		{"a gate:self key from its non-declarer", authority, extapi.KindAvailability, 6,
			`{"rows":[{"down_access":"locked","feature":"acme.tools","steps":[{"access":"full"}]}]}`, messages.ErrorPluginStateNotOwner, extapi.ReasonPermissionDenied},
		{"a code the manifest does not list", authority, extapi.KindAvailability, 6,
			`{"rows":[{"down_access":"locked","feature":"acme.backup","steps":[{"access":"locked","notice":{"code":"SOMEONE_ELSES"}}]}]}`, messages.ErrorPluginStateNotOwner, extapi.ReasonPermissionDenied},
		{"node holds without the permission", pusherOf(acmeOps), extapi.KindNodeHolds, 1, `{"nodes":{"3":{"code":"ACME_MAINTENANCE"}}}`, messages.ErrorPluginStateNotPermitted, extapi.ReasonPermissionDenied},
		{"fleet capacity without the table grant", Pusher{ID: "acme_ops", Publisher: "acme", Codes: acmeCodes, Generation: 1}, extapi.KindFleetCapacity, 1, capacityBody(2, 16), messages.ErrorPluginStateNotPermitted, extapi.ReasonPermissionDenied},
		{"attestations from a package that is no authority", pusherOf(acmeOps), extapi.KindAttestations, 1, `{"attestations":[{"blob":"AA==","format":"x","name":"a"}]}`, messages.ErrorPluginStateNotPermitted, extapi.ReasonPermissionDenied},
		{"an attestation about a key it does not answer for", pusherOf(acmeAuthority), extapi.KindAttestations, 1, `{"attestations":[{"blob":"AA==","feature":"acme.tools","format":"x","name":"a"}]}`, messages.ErrorPluginStateNotOwner, extapi.ReasonPermissionDenied},
		{"too many holds", pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, manyHolds(extapi.MaxNodeHolds + 1), extapi.ErrorStateTooLarge, extapi.ReasonInvalidArgument},
		{"a row breaking its rule", authority, extapi.KindBanners, 1, `{"banners":[{"audience":"everyone","id":"b","steps":[{"hidden":true}]}]}`, extapi.ErrorStateRejected, extapi.ReasonInvalidArgument},
		{"an unknown field", authority, extapi.KindBanners, 1, `{"banners":[],"extra":1}`, extapi.ErrorWireUnknownField, extapi.ReasonInvalidArgument},
		{"a schema this build does not read", authority, extapi.KindBanners, 1, `{}`, extapi.ErrorSchemaTooNew, extapi.ReasonUnsupported},
		{"a package that is not installed", Pusher{ID: "acme_gone", Publisher: "acme", Grants: acmeGrants, Codes: acmeCodes, Generation: 1}, extapi.KindAvailability, 1, `{}`, messages.ErrorPluginStateNotOwner, extapi.ReasonPermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			push := &extapi.StatePush{Kind: tc.kind, Schema: 1, Generation: tc.gen, Body: json.RawMessage(tc.body)}
			if tc.code == extapi.ErrorSchemaTooNew {
				push.Schema = 2
			}
			ack, w := h.s.Push(context.Background(), tc.pusher, push)
			if w == nil || w.Code != tc.code || w.Reason != tc.reason || ack != nil {
				t.Fatalf("push = %+v, %+v; want %s/%s", ack, w, tc.code, tc.reason)
			}
			if err := w.Validate(); err != nil {
				t.Fatalf("error body does not validate: %v", err)
			}
		})
	}
	if got := h.s.Availability("acme.backup"); got.Access != featuregate.Full {
		t.Fatalf("a refused push changed the snapshot: %+v", got)
	}
	if _, w := h.s.Push(context.Background(), authority, &extapi.StatePush{Kind: extapi.KindBanners, Schema: 1, Generation: 1, Body: make(json.RawMessage, extapi.MaxPushBytes+1)}); w == nil || w.Code != extapi.ErrorStateTooLarge {
		t.Fatalf("oversized body: %+v", w)
	}
	if _, w := h.s.Push(context.Background(), authority, nil); w == nil {
		t.Fatal("a nil push was accepted")
	}
}

func TestAPushOverAReplacedProcessIsStale(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.s.OwnerUp("acme_authority", 3)
	old := pusherOf(acmeAuthority)
	old.Generation = 2
	if _, w := h.try(old, extapi.KindAvailability, 1, backupFull); w == nil || w.Code != messages.ErrorPluginStateStale {
		t.Fatalf("old process push = %+v", w)
	}
	current := old
	current.Generation = 3
	h.push(current, extapi.KindAvailability, 1, backupFull)
}

func TestPushesOfOneTickCommitInOneTransactionAndTheLatestWins(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load()
	booted := st.writes.Load()
	authority, ops := pusherOf(acmeAuthority), pusherOf(acmeOps, "push:node_holds")
	type result struct {
		ack *extapi.StateAck
		err *extapi.WireError
	}
	results := make(chan result, 3)
	go func() {
		a, w := h.try(authority, extapi.KindAvailability, 1, backupFull)
		results <- result{a, w}
	}()
	h.waitFor("the first push to queue", func() bool { h.s.mu.Lock(); defer h.s.mu.Unlock(); return len(h.s.pending) == 1 })
	go func() {
		a, w := h.try(authority, extapi.KindAvailability, 2, `{"rows":[{"down_access":"locked","feature":"acme.backup","steps":[{"access":"locked"}]}]}`)
		results <- result{a, w}
	}()
	go func() {
		a, w := h.try(ops, extapi.KindNodeHolds, 1, `{"nodes":{"7":{"code":"ACME_MAINTENANCE"}}}`)
		results <- result{a, w}
	}()
	h.waitFor("the other pushes to queue", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		p := h.s.pending[ownerKind{"acme_authority", extapi.KindAvailability}]
		return len(h.s.pending) == 2 && p != nil && p.part.gen == 2
	})
	h.run()
	var superseded, applied int
	for range 3 {
		r := <-results
		switch {
		case r.err != nil:
			t.Fatalf("refused: %+v", r.err)
		case r.ack.SupersededBy == 2 && r.ack.Generation == 1:
			superseded++
		case r.ack.AppliedAt != nil:
			applied++
		}
	}
	if superseded != 1 || applied != 2 {
		t.Fatalf("superseded=%d applied=%d", superseded, applied)
	}
	if n := st.writes.Load() - booted; n != 1 {
		t.Fatalf("three pushes took %d writes, want one transaction", n)
	}
	if h.s.Availability("acme.backup").Access != featuregate.Locked || !h.s.Hold(7, 8).held() {
		t.Fatal("the tick's pushes were not all applied")
	}
}

func (h Hold) held() bool { return h.Code != "" }

func TestAFailedCommitAppliesNothingAndTheRetryDoes(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	st.failWrites.Store(1)
	_, w := h.try(pusherOf(acmeAuthority), extapi.KindAvailability, 1, backupFull)
	if w == nil || w.Code != messages.ErrorPluginStateNotSaved || w.Reason != extapi.ReasonUnavailable || w.RetryAfterMS == 0 {
		t.Fatalf("failed commit = %+v", w)
	}
	if h.s.Availability("acme.backup").Access != featuregate.Locked || len(h.s.Held("acme_authority")) != 0 {
		t.Fatal("a push whose commit failed was applied")
	}
	h.push(pusherOf(acmeAuthority), extapi.KindAvailability, 1, backupFull)
	if h.s.Availability("acme.backup").Access != featuregate.Full {
		t.Fatal("the retried push was not applied")
	}
}

func TestPushWaitEndsWithItsContext(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load() // no tick running
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, w := h.s.Push(ctx, pusherOf(acmeAuthority), &extapi.StatePush{Kind: extapi.KindAvailability, Schema: 1, Generation: 1, Body: json.RawMessage(backupFull)})
	if w == nil || w.Reason != extapi.ReasonCancelled {
		t.Fatalf("push without a tick = %+v", w)
	}
}

func capacityBody(nodes, cards int) string {
	return `{"hold":{"code":"ACME_OVER_CAPACITY"},"refuse_card":{"code":"ACME_CARD_LIMIT","params":{"max":` + strconv.Itoa(cards) + `}},"refuse_node":{"code":"ACME_NODE_LIMIT","params":{"max":` + strconv.Itoa(nodes) + `}},"select":"enrollment_order","steps":[{"max_cards":` + strconv.Itoa(cards) + `,"max_nodes_with_cards":` + strconv.Itoa(nodes) + `}]}`
}
