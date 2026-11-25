//go:build linux && dqlite

package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestRefusals(every time.Duration) (*businessRefusals, *bytes.Buffer) {
	out := &bytes.Buffer{}
	b := &businessRefusals{recoveryEvery: every}
	b.log.Store(slog.New(slog.NewTextHandler(out, nil)))
	return b, out
}

// Within a run a reason is logged at its first, second, fourth refusal and so
// on; a served request ends the run with one line; the next run starts its
// warnings again, however many refusals the process has seen before.
func TestBusinessRefusalsAreCountedAndLoggedByReason(t *testing.T) {
	b, out := newTestRefusals(time.Nanosecond)
	b.served()
	if out.Len() != 0 {
		t.Fatalf("a served request with no refusal before it logged:\n%s", out.String())
	}
	for range 5 {
		b.record(refusalNoKnownLeader)
	}
	b.record(refusalNoGeneration)
	if got := b.snapshot(); got[refusalNoKnownLeader] != 5 || got[refusalNoGeneration] != 1 || len(got) != 2 {
		t.Fatalf("counts = %v", got)
	}
	text := out.String()
	for _, want := range []string{
		"reason=no_known_leader refused=1 since_start=1", "reason=no_known_leader refused=2 since_start=2",
		"reason=no_known_leader refused=4 since_start=4", "reason=no_business_generation refused=1 since_start=1",
	} {
		if strings.Count(text, want) != 1 {
			t.Fatalf("want one line with %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "level=WARN"); got != 4 {
		t.Fatalf("%d warnings for 6 refusals of 2 reasons, want 4:\n%s", got, text)
	}
	out.Reset()
	b.served()
	b.served()
	text = out.String()
	if strings.Count(text, "business served again after refusals") != 1 || !strings.Contains(text, "level=INFO") || !strings.Contains(text, "refused=6 lasted_ms=") {
		t.Fatalf("serving again after 6 refusals logged:\n%s", text)
	}
	// A later incident is announced from its first refusal, although the
	// reason's count since the start is not a power of two.
	time.Sleep(time.Millisecond)
	out.Reset()
	for range 3 {
		b.record(refusalNoKnownLeader)
	}
	text = out.String()
	if strings.Count(text, "level=WARN") != 2 || !strings.Contains(text, "refused=1 since_start=6") || !strings.Contains(text, "refused=2 since_start=7") {
		t.Fatalf("the second run logged:\n%s", text)
	}
	out.Reset()
	b.served()
	if text = out.String(); !strings.Contains(text, "refused=3 lasted_ms=") {
		t.Fatalf("the second run ended with:\n%s", text)
	}
	if got := b.snapshot()[refusalNoKnownLeader]; got != 8 {
		t.Fatalf("count since the start = %d, want 8", got)
	}
}

// A member that alternates between refusing and serving leaves one recovery
// line per interval and a bounded number of warnings, and the line that comes
// carries everything refused since the one before.
func TestFlappingBusinessLeavesOneRecoveryLinePerInterval(t *testing.T) {
	b, out := newTestRefusals(time.Hour)
	b.record(refusalNoKnownLeader)
	b.served()
	if got := strings.Count(out.String(), "business served again after refusals"); got != 1 {
		t.Fatalf("the first recovery logged %d lines:\n%s", got, out.String())
	}
	out.Reset()
	for range 100 {
		b.record(refusalNoKnownLeader)
		b.served()
	}
	text := out.String()
	if strings.Contains(text, "business served again after refusals") {
		t.Fatalf("recovery lines inside the interval:\n%s", text)
	}
	// 100 refusals in one run: the 1st, 2nd, 4th, ..., 64th.
	if got := strings.Count(text, "level=WARN"); got != 7 {
		t.Fatalf("%d warnings for 100 alternations, want 7:\n%s", got, text)
	}
	b.mu.Lock()
	pending := b.pending
	b.mu.Unlock()
	if !pending {
		t.Fatal("no recovery line is scheduled for the end of the interval")
	}
	// The interval ends while business is served: one line for the whole run.
	out.Reset()
	b.reportLater()
	if text = out.String(); strings.Count(text, "business served again after refusals") != 1 || !strings.Contains(text, "refused=100 lasted_ms=") {
		t.Fatalf("the scheduled line:\n%s", text)
	}
	// The interval ends while business is refused: nothing yet.
	b.record(refusalNoKnownLeader)
	out.Reset()
	b.reportLater()
	if out.Len() != 0 {
		t.Fatalf("a recovery line while business is refused:\n%s", out.String())
	}
}

// Every refusal the gate and the member handler give names its reason in the
// header, keeps the body clients match on, and is counted once; a probe gets
// the header and no count; a served request and the liveness probe get
// neither.
func TestGateAndMemberHandlerRefusalsNameTheirReason(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	gate := func(b *businessRefusals) http.Handler {
		gate := newBusinessHandlerGate()
		gate.refusals = b
		return gate
	}
	upgrading := func(b *businessRefusals) http.Handler {
		gate := newBusinessHandlerGate()
		gate.refusals = b
		gate.clusterUpgrade.Store(true)
		return gate
	}
	cases := []struct {
		name    string
		handler func(*businessRefusals) http.Handler
		path    string
		status  int
		reason  string
		counted bool
		body    string
	}{
		{"gate without a generation", gate, "/api/v1/nodes", 503, refusalNoGeneration, true, "member business service is not ready"},
		{"gate without a generation, readiness probe", gate, "/api/v1/readyz", 503, refusalNoGeneration, false, "member business service is not ready"},
		{"gate closed for a cluster upgrade", upgrading, "/api/v1/nodes", 503, refusalClusterUpgrade, true, "CLUSTER_UPGRADE_IN_PROGRESS"},
		{"gate liveness", gate, "/healthz", 200, "", false, ""},
		{"member handler without business", func(b *businessRefusals) http.Handler { return memberServiceHandler(nil, nil, b) }, "/api/v1/nodes", 503, refusalNoHandler, true, "member business service is not ready"},
		{"member handler without a member", func(b *businessRefusals) http.Handler { return memberServiceHandler(ok, nil, b) }, "/api/v1/nodes", 503, refusalNoHandler, true, "member business service is not ready"},
		{"member handler liveness", func(b *businessRefusals) http.Handler { return memberServiceHandler(ok, nil, b) }, "/healthz", 200, "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newTestRefusals(time.Nanosecond)
			rec := httptest.NewRecorder()
			tc.handler(b).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("answered %d %q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get(businessRefusalHeader); got != tc.reason {
				t.Fatalf("refusal header %q, want %q", got, tc.reason)
			}
			counts := b.snapshot()
			if tc.counted && (len(counts) != 1 || counts[tc.reason] != 1) {
				t.Fatalf("counts = %v, want one %s", counts, tc.reason)
			}
			if !tc.counted && len(counts) != 0 {
				t.Fatalf("counts = %v, want none", counts)
			}
		})
	}
}

// The header carries a value of the one list and nothing else: each reason
// the gate and the proof give in their own words has its code there, and a
// reason nobody listed is answered as "other", never passed through.
func TestRefusalHeaderCarriesOnlyListedCodes(t *testing.T) {
	seen := map[string]bool{}
	for _, code := range businessRefusalCodes {
		if seen[code] || code == "" || strings.ContainsAny(code, " :/.") {
			t.Fatalf("code %q is repeated or not a plain word", code)
		}
		seen[code] = true
	}
	for word, code := range gateWordCodes {
		if seen[word] || !seen[code] {
			t.Fatalf("the gate's word %q is itself a code, or its code %q is not listed", word, code)
		}
	}
	want := map[string]string{
		"no_business_generation": "no_business_generation", "generation_changed": "generation_changed",
		"proof_missing": "proof_missing", "expired": "proof_expired", "stopped": "proof_stopped",
		"run_canceled": "proof_run_canceled", "proof_canceled": "proof_canceled", "headroom": "proof_headroom",
		"epoch_changed": "proof_epoch_changed", "proof_owner_changed": "proof_owner_changed",
		"renewal_unavailable": "renewal_unavailable", "request_canceled": "request_canceled", "request_deadline": "request_deadline",
		"10.0.0.7:9002 refused": "other", "": "other",
	}
	for word := range gateWordCodes {
		if _, named := want[word]; !named {
			t.Fatalf("this test does not name the gate's word %q", word)
		}
	}
	gate := newBusinessHandlerGate()
	for reason, code := range want {
		if !seen[code] {
			t.Fatalf("%q maps to %q, which is not in the list", reason, code)
		}
		b, _ := newTestRefusals(time.Nanosecond)
		gate.refusals = b
		rec := httptest.NewRecorder()
		gate.refuse(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil), reason)
		if got := rec.Header().Get(businessRefusalHeader); got != code || rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%q: header %q status %d, want %q", reason, got, rec.Code, code)
		}
		if len(b.snapshot()) != 0 {
			t.Fatalf("%q: a readiness probe was counted", reason)
		}
	}
	rec := httptest.NewRecorder()
	(*businessRefusals)(nil).mark(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil), "member 3 at 10.0.0.7")
	if got := rec.Header().Get(businessRefusalHeader); got != refusalOther {
		t.Fatalf("an unlisted reason reached the header as %q", got)
	}
}

// A request that reaches business closes a run of refusals and is not itself
// counted; a probe that reaches it does not close the run.
func TestServedBusinessEndsARunOfRefusals(t *testing.T) {
	b, out := newTestRefusals(time.Nanosecond)
	served := b.serving(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	b.record(refusalNoKnownLeader)
	out.Reset()
	served.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
	if out.Len() != 0 || !b.open.Load() {
		t.Fatalf("a probe ended the run:\n%s", out.String())
	}
	served.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if b.open.Load() || strings.Count(out.String(), "business served again after refusals") != 1 {
		t.Fatalf("a served request did not end the run:\n%s", out.String())
	}
	if got := b.snapshot(); len(got) != 1 || got[refusalNoKnownLeader] != 1 {
		t.Fatalf("serving changed the counts: %v", got)
	}
	if b.serving(nil) != nil {
		t.Fatal("serving(nil) must stay nil: the member handler refuses a missing business handler")
	}
}

// One evaluation says whether a member is business-ready and, when it is
// not, the first fact it is missing: the code is empty exactly when the
// member is ready, for every combination of the facts.
func TestMemberServiceNamesTheMissingFact(t *testing.T) {
	if got := (*App)(nil).memberService(t.Context(), false); got.BusinessReady || got.BusinessRefusal != refusalNoHandler {
		t.Fatalf("no member: %+v", got)
	}
	for _, tc := range []struct {
		live, materials, sessions, target bool
		refusal                           string
	}{
		{true, true, true, true, ""},
		{false, true, true, true, refusalNoHandler},
		{true, false, true, true, refusalProofNotEligible},
		{true, true, false, true, refusalSessionsUnroutable},
		{true, true, true, false, refusalTargetNotCurrent},
		{true, false, false, false, refusalProofNotEligible},
		{false, false, false, false, refusalNoHandler},
	} {
		got := EvaluateMemberService(tc.live, tc.materials, false, tc.sessions, tc.target)
		if got.BusinessRefusal != tc.refusal || got.BusinessReady != (tc.refusal == "") {
			t.Fatalf("facts %+v: %+v", tc, got)
		}
	}
}

// The record a member's runtime holds is the one its gate and its member
// handler count in: nothing is kept anywhere else in the process.
func TestMemberRuntimeHandsItsRecordToWhateverRefuses(t *testing.T) {
	b, _ := newTestRefusals(time.Nanosecond)
	runtime := &memberRuntime{member: closedMember{cfg: Config{}}, refusals: b}
	closed := newWaitingClosedMemberApp(runtime, nil)
	closed.businessHTTP.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	runtime.handler(ok).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	counts := b.snapshot()
	// A waiting member's gate requires a material proof and has none.
	if len(counts) != 2 || counts[refusalProofMissing] != 1 || counts[refusalNoHandler] != 1 {
		t.Fatalf("counts = %v, want one %s from the gate and one %s from the member handler", counts, refusalProofMissing, refusalNoHandler)
	}
	// A runtime without a record still names the reason and counts nothing.
	rec := httptest.NewRecorder()
	(&memberRuntime{}).handler(ok).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if got := rec.Header().Get(businessRefusalHeader); got != refusalNoHandler || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a record: %d, refusal %q", rec.Code, got)
	}
}
