//go:build linux && dqlite

package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestHealthyFollowerAcceptsTheEngineCatalog(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "member.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	leader := false
	st.SetLocalSchedulingLeader(func(context.Context) (bool, error) { return leader, nil })
	signer, token := memberAdminSession(t, st, "member-service-entry")
	member := &App{store: st}
	entry := memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), member)

	public := requestMemberEntry(entry, http.MethodGet, "/healthz", "")
	if public.Code != http.StatusOK {
		t.Fatalf("public probe status = %d, body %s", public.Code, public.Body.String())
	}
	var publicBody map[string]any
	if err := json.Unmarshal(public.Body.Bytes(), &publicBody); err != nil {
		t.Fatal(err)
	}
	if publicBody["status"] != "ok" {
		t.Fatalf("public probe status field = %#v", publicBody["status"])
	}
	for _, key := range []string{"live", "business_ready", "schedulable", "entry_candidate"} {
		if _, ok := publicBody[key]; ok {
			t.Fatalf("public probe included %s: %s", key, public.Body.String())
		}
	}
	t.Log("public /healthz stays the liveness payload")

	open := requestMemberEntry(entry, http.MethodGet, "/api/v1/healthz", "")
	if open.Code != http.StatusUnauthorized {
		t.Fatalf("management health without a token = %d, body %s", open.Code, open.Body.String())
	}
	if strings.Contains(open.Body.String(), "business_ready") || strings.Contains(open.Body.String(), "schedulable") || strings.Contains(open.Body.String(), "entry_candidate") {
		t.Fatalf("unauthenticated management health exposed member answers: %s", open.Body.String())
	}
	t.Log("management health without a token stays 401")

	follower := readMemberHealth(t, entry, token)
	if !follower.Live || !follower.BusinessReady || follower.Schedulable {
		t.Fatalf("healthy follower answers = %+v", follower)
	}
	// The count of refused business per reason is part of the authenticated
	// answer once there is one, and never of the other two.
	memberBusinessRefusals.record(refusalNoKnownLeader)
	counted := requestMemberEntry(entry, http.MethodGet, "/api/v1/healthz", token)
	var withRefusals struct {
		Refusals map[string]uint64 `json:"business_refusals"`
	}
	if err := json.Unmarshal(counted.Body.Bytes(), &withRefusals); err != nil || withRefusals.Refusals[refusalNoKnownLeader] == 0 {
		t.Fatalf("authenticated health lacks business_refusals: %v %s", err, counted.Body.String())
	}
	for _, path := range []string{"/healthz", "/api/v1/healthz"} {
		if anonymous := requestMemberEntry(entry, http.MethodGet, path, ""); strings.Contains(anonymous.Body.String(), "business_refusals") {
			t.Fatalf("%s without a token exposed the refusal counts: %s", path, anonymous.Body.String())
		}
	}
	engines := requestMemberEntry(entry, http.MethodGet, "/api/v1/engines", token)
	var catalog struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if engines.Code != http.StatusOK || json.Unmarshal(engines.Body.Bytes(), &catalog) != nil || len(catalog.Items) == 0 || catalog.Total != len(catalog.Items) || catalog.Items[0].Name == "" {
		t.Fatalf("follower engine catalog = %d %.300s", engines.Code, engines.Body.String())
	}
	t.Log("healthy follower is live and business-ready, is not the scheduler, and the engine catalog returns 200")

	leader = true
	setMaterialEligibility(member, newMaterialEligibility(time.Now, func(context.Context) (bool, error) {
		return false, nil
	}, time.Second))
	withoutMaterials := readMemberHealth(t, entry, token)
	if !withoutMaterials.Live || withoutMaterials.BusinessReady || !withoutMaterials.Schedulable {
		t.Fatalf("leader without materials = %+v", withoutMaterials)
	}
	refused := requestMemberEntry(entry, http.MethodGet, "/api/v1/engines", token)
	if refused.Code != http.StatusServiceUnavailable || !strings.Contains(refused.Body.String(), "member business service is not ready") {
		t.Fatalf("leader without materials catalog = %d %s", refused.Code, refused.Body.String())
	}
	stillUp := requestMemberEntry(entry, http.MethodGet, "/healthz", "")
	if stillUp.Code != http.StatusOK || strings.Contains(stillUp.Body.String(), "business_ready") || strings.Contains(stillUp.Body.String(), "entry_candidate") {
		t.Fatalf("liveness changed while business was refused: %d %s", stillUp.Code, stillUp.Body.String())
	}
	t.Log("scheduling leader without a current material proof stays schedulable, and the engine catalog returns 503")

	setMaterialEligibility(member, nil)
	st.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 3, Key: []byte("member-key")})
	withoutSession := readMemberHealth(t, entry, token)
	if !withoutSession.Live || withoutSession.BusinessReady || !withoutSession.Schedulable {
		t.Fatalf("leader without a session listen = %+v", withoutSession)
	}
	refusedSession := requestMemberEntry(entry, http.MethodGet, "/api/v1/engines", token)
	if refusedSession.Code != http.StatusServiceUnavailable {
		t.Fatalf("leader without a session listen catalog = %d %s", refusedSession.Code, refusedSession.Body.String())
	}
	t.Log("scheduling leader whose session listen is empty stays schedulable, and the engine catalog returns 503")

	st.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 3, Key: []byte("member-key"), Listen: "127.0.0.1:9101"})
	ready := readMemberHealth(t, entry, token)
	if !ready.Live || !ready.BusinessReady || !ready.Schedulable {
		t.Fatalf("ready leader = %+v", ready)
	}
	accepted := requestMemberEntry(entry, http.MethodGet, "/api/v1/engines", token)
	if accepted.Code != http.StatusOK {
		t.Fatalf("ready leader catalog = %d %s", accepted.Code, accepted.Body.String())
	}
	t.Log("scheduling leader with materials and a session listen is live, business-ready, and schedulable, and the engine catalog returns 200")
}

type memberAnswers struct {
	Live          bool
	BusinessReady bool
	Schedulable   bool
}

func readMemberHealth(t *testing.T, entry http.Handler, token string) memberAnswers {
	t.Helper()
	rec := requestMemberEntry(entry, http.MethodGet, "/api/v1/healthz", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("management health = %d %s", rec.Code, rec.Body.String())
	}
	var payload memberHealthWire
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Status != "ok" || payload.Live == nil || payload.BusinessReady == nil || payload.Schedulable == nil {
		t.Fatalf("management health = %s", rec.Body.String())
	}
	return memberAnswers{Live: *payload.Live, BusinessReady: *payload.BusinessReady, Schedulable: *payload.Schedulable}
}

func setMaterialEligibility(member *App, eligibility *materialEligibility) {
	member.materialEligibilityMu.Lock()
	member.materialEligibility = eligibility
	member.materialEligibilityMu.Unlock()
}

func TestFailedEntryCheckLeavesTheCandidateSet(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "member.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	leader := false
	st.SetLocalSchedulingLeader(func(context.Context) (bool, error) { return leader, nil })
	signer, token := memberAdminSession(t, st, "entry-check")
	member := &App{store: st}
	entry := memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), member)

	public := requestMemberEntry(entry, http.MethodGet, "/healthz", "")
	if public.Code != http.StatusOK || strings.Contains(public.Body.String(), "entry_candidate") {
		t.Fatalf("public probe = %d %s", public.Code, public.Body.String())
	}
	t.Log("public /healthz stays the liveness payload")

	open := requestMemberEntry(entry, http.MethodGet, "/api/v1/healthz", "")
	if open.Code != http.StatusUnauthorized || strings.Contains(open.Body.String(), "entry_candidate") {
		t.Fatalf("management health without a token = %d %s", open.Code, open.Body.String())
	}

	follower := readEntryHealth(t, entry, token)
	if !follower.Live || !follower.BusinessReady || follower.Schedulable || !follower.EntryCandidate {
		t.Fatalf("follower without a check = %+v", follower)
	}
	t.Log("a business-ready follower with no entry check stays a candidate and is not the scheduler")

	member.SetEntryCheck(func(context.Context) error { return nil }, time.Second)
	passed := readEntryHealth(t, entry, token)
	if !passed.Live || !passed.BusinessReady || passed.Schedulable || !passed.EntryCandidate {
		t.Fatalf("finished check on a follower = %+v", passed)
	}
	t.Log("a check that finishes in time leaves the follower in the candidate set without scheduling authority")

	var calls atomic.Int32
	member.SetEntryCheck(func(context.Context) error {
		calls.Add(1)
		return errors.New("entry check failed")
	}, time.Second)
	engines := requestMemberEntry(entry, http.MethodGet, "/api/v1/engines", token)
	if engines.Code != http.StatusOK || calls.Load() != 0 {
		t.Fatalf("engine catalog = %d, check calls = %d, body %s", engines.Code, calls.Load(), engines.Body.String())
	}
	failed := readEntryHealth(t, entry, token)
	if calls.Load() != 1 || failed.EntryCandidate || !failed.Live || !failed.BusinessReady || failed.Schedulable {
		t.Fatalf("failed check = %+v calls=%d", failed, calls.Load())
	}
	t.Log("a failed check removes the follower from the candidates while liveness, business readiness, and scheduling stay unchanged, and the engine catalog still returns 200")

	member.SetEntryCheck(func(context.Context) error { return nil }, 0)
	noTime := readEntryHealth(t, entry, token)
	if noTime.EntryCandidate || !noTime.BusinessReady || noTime.Schedulable {
		t.Fatalf("check with no time left = %+v", noTime)
	}
	t.Log("a check that has no time to finish removes the follower from the candidates")

	leader = true
	member.SetEntryCheck(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, time.Millisecond)
	timedOut := readEntryHealth(t, entry, token)
	if timedOut.EntryCandidate || !timedOut.Live || !timedOut.BusinessReady || !timedOut.Schedulable {
		t.Fatalf("timed out check on the scheduling leader = %+v", timedOut)
	}
	t.Log("a check that does not finish before its deadline removes the scheduling leader from the candidates without a leadership change")

	member.SetEntryCheck(func(context.Context) error { return nil }, time.Second)
	setMaterialEligibility(member, newMaterialEligibility(time.Now, func(context.Context) (bool, error) {
		return false, nil
	}, time.Second))
	notReady := readEntryHealth(t, entry, token)
	if notReady.EntryCandidate || notReady.BusinessReady || !notReady.Schedulable {
		t.Fatalf("leader without materials = %+v", notReady)
	}
	refused := requestMemberEntry(entry, http.MethodGet, "/api/v1/engines", token)
	if refused.Code != http.StatusServiceUnavailable {
		t.Fatalf("leader without materials catalog = %d %s", refused.Code, refused.Body.String())
	}
	t.Log("a finished check does not keep a scheduling leader in the candidate set when the material proof is not current")

	setMaterialEligibility(member, nil)
	restored := readEntryHealth(t, entry, token)
	if !restored.EntryCandidate || !restored.Live || !restored.BusinessReady || !restored.Schedulable {
		t.Fatalf("restored leader = %+v", restored)
	}
	t.Log("a later check that finishes puts the business-ready scheduling leader back in the candidate set")

	member.SetEntryCheck(nil, time.Second)
	cleared := readEntryHealth(t, entry, token)
	if !cleared.EntryCandidate || !cleared.Schedulable {
		t.Fatalf("cleared check = %+v", cleared)
	}
	t.Log("clearing the check leaves the business-ready member in the candidate set")
}

type entryAnswers struct {
	Live           bool
	BusinessReady  bool
	Schedulable    bool
	EntryCandidate bool
}

func readEntryHealth(t *testing.T, entry http.Handler, token string) entryAnswers {
	t.Helper()
	rec := requestMemberEntry(entry, http.MethodGet, "/api/v1/healthz", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("management health = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "priority") {
		t.Fatalf("management health kept a priority: %s", rec.Body.String())
	}
	var payload memberHealthWire
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Status != "ok" || payload.Live == nil || payload.BusinessReady == nil || payload.Schedulable == nil || payload.EntryCandidate == nil {
		t.Fatalf("management health = %s", rec.Body.String())
	}
	return entryAnswers{
		Live:           *payload.Live,
		BusinessReady:  *payload.BusinessReady,
		Schedulable:    *payload.Schedulable,
		EntryCandidate: *payload.EntryCandidate,
	}
}

func requestMemberEntry(entry http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	entry.ServeHTTP(rec, req)
	return rec
}

// memberHealthWire is the authenticated health document a member answers:
// the server's health fields plus the member's own answers.
type memberHealthWire struct {
	Status         string `json:"status"`
	Live           *bool  `json:"live"`
	BusinessReady  *bool  `json:"business_ready"`
	Schedulable    *bool  `json:"schedulable"`
	EntryCandidate *bool  `json:"entry_candidate"`
}
