//go:build linux && dqlite

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// heldSource is a member whose status report takes as long as the test
// says, whatever its context: a disk or a probe that does not answer.
type heldSource struct {
	diagnostics.MemberSource
	entered chan struct{}
	release chan struct{}
	delay   time.Duration
}

func (h *heldSource) Status(ctx context.Context) string {
	if h.entered != nil {
		select {
		case h.entered <- struct{}{}:
		default:
		}
	}
	if h.release != nil {
		<-h.release
	}
	time.Sleep(h.delay)
	return "Overall: held\n"
}

func holdDiagnosticsSource(t *testing.T, h *heldSource) {
	t.Helper()
	previous := clusterDiagnosticsSource
	clusterDiagnosticsSource = func(cfg Config) diagnostics.MemberSource {
		h.MemberSource = previous(cfg)
		return h
	}
	t.Cleanup(func() { clusterDiagnosticsSource = previous })
}

func readPart(t *testing.T, body io.ReadCloser) (int, error) {
	t.Helper()
	defer body.Close()
	raw, err := io.ReadAll(body)
	return len(raw), err
}

// The listener cuts every answer at its write timeout; a part takes longer
// than that when the member's status probes time out (a member down, no
// leader), so the route sets its own deadline.
func TestADiagnosticsPartOutlivesTheListenersWriteTimeout(t *testing.T) {
	previous := enrollmentWriteTimeout
	enrollmentWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { enrollmentWriteTimeout = previous })
	holdDiagnosticsSource(t, &heldSource{delay: 1200 * time.Millisecond})
	now := time.Now().UTC()
	f := newClosedFactsTLSFixture(t)
	server, _ := diagnosticsMember(t, f, now)
	cert, key := memberPEM(t, f.requesterPair)
	answer, err := diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	files := partArchive(t, answer.Body)
	if !strings.Contains(files[diagnostics.MemberStatusFile], "Overall: held") || !strings.Contains(files[diagnostics.MemberLogFile], "event=step_down") {
		t.Fatalf("the part was cut at the listener's write timeout: %d files", len(files))
	}
}

// One part at a time: a second asker is told 429 while the first is read.
func TestAMemberGivesOneDiagnosticsPartAtATime(t *testing.T) {
	held := &heldSource{entered: make(chan struct{}, 1), release: make(chan struct{})}
	holdDiagnosticsSource(t, held)
	now := time.Now().UTC()
	f := newClosedFactsTLSFixture(t)
	server, _ := diagnosticsMember(t, f, now)
	cert, key := memberPEM(t, f.requesterPair)
	first, err := diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	<-held.entered
	_, err = diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, now.Add(-time.Hour), now)
	var status *enrollment.StatusError
	if !errors.As(err, &status) || status.Code != http.StatusTooManyRequests {
		t.Fatalf("second asker: %v, want 429", err)
	}
	if !errors.Is(memberAnswerError(err), diagnostics.ErrMemberBusy) {
		t.Fatalf("429 is not read as busy: %v", memberAnswerError(err))
	}
	close(held.release)
	if n, err := readPart(t, first.Body); err != nil || n == 0 {
		t.Fatalf("first part: %d bytes, %v", n, err)
	}
	// The slot is free again.
	deadline := time.Now().Add(3 * time.Second)
	for {
		again, err := diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, now.Add(-time.Hour), now)
		if err == nil {
			_, _ = readPart(t, again.Body)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slot was not given back: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A part being read from a disk that does not answer must not hold up the
// member's own lifecycle: closing the listener ends the request at once.
func TestAStuckDiagnosticsPartDoesNotHoldTheListenersClose(t *testing.T) {
	held := &heldSource{entered: make(chan struct{}, 1), release: make(chan struct{})}
	holdDiagnosticsSource(t, held)
	t.Cleanup(func() { close(held.release) })
	now := time.Now().UTC()
	f := newClosedFactsTLSFixture(t)
	server, _ := diagnosticsMember(t, f, now)
	cert, key := memberPEM(t, f.requesterPair)
	answer, err := diagnosticsClient(t, f, server).OpenClusterDiagnostics(t.Context(), cert, key, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	<-held.entered
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatalf("the listener's close waited for the stuck part: %v", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the listener's close took %v with a part stuck", took)
	}
	// The asker sees a part that ended early, not a whole one.
	if _, err := readPart(t, answer.Body); err == nil {
		t.Fatal("the cut part read as whole")
	}
}

func TestAMembersRefusalIsSortedByItsStatus(t *testing.T) {
	for code, want := range map[int]error{
		http.StatusForbidden: diagnostics.ErrMemberRefused, http.StatusTooManyRequests: diagnostics.ErrMemberBusy,
		http.StatusNotFound: diagnostics.ErrMemberUnsupported, http.StatusServiceUnavailable: diagnostics.ErrMemberFailed,
		http.StatusInternalServerError: diagnostics.ErrMemberFailed,
	} {
		if got := memberAnswerError(&enrollment.StatusError{Code: code}); !errors.Is(got, want) {
			t.Fatalf("status %d = %v, want %v", code, got, want)
		}
	}
	plain := errors.New("dial tcp: connection refused")
	if got := memberAnswerError(plain); got != plain {
		t.Fatalf("an unreached member = %v", got)
	}
}

// collectingMember is member 9 of the fixture as `gpuworks support-bundle`
// finds it: its data directory, no database. The leader's view, when the
// test gives one, names member 7's listener and key after a delay (the
// time this side spends finding the members before any request leaves).
func collectingMember(t *testing.T, f *closedFactsTLSFixture, origin, key string, preamble time.Duration) ClusterMemberDiagnosticsRequest {
	t.Helper()
	root := writeStatusRaft(t, statusIdentity(9, "127.0.0.1:9911", 0), fixtureMembers(), map[string]string{"dqlite-lock": ""})
	writeMemberIdentity(t, f, root, closedStatusPort(t), f.requesterPair)
	previous, previousProof := statusQuorumFetch, statusQuorumProofRead
	t.Cleanup(func() { statusQuorumFetch, statusQuorumProofRead = previous, previousProof })
	none := func(context.Context, Config, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		return enrollment.ClusterQuorumFacts{}, false
	}
	statusQuorumProofRead = none
	statusQuorumFetch = func(_ context.Context, s clusterStatus, _ time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		if s.id != 9 || origin == "" {
			return enrollment.ClusterQuorumFacts{}, false
		}
		time.Sleep(preamble)
		return enrollment.ClusterQuorumFacts{LeaderID: 7, Term: 4, Index: 42, ConfigurationIndex: 11, Members: []enrollment.ClusterMember{
			{ID: 7, Address: "127.0.0.1:9910", Role: "voter", KeyFingerprint: key, EnrollmentOrigin: origin},
			{ID: 9, Address: "127.0.0.1:9911", Role: "voter", KeyFingerprint: f.requesterKey},
			{ID: 11, Address: "127.0.0.1:9912", Role: "voter", KeyFingerprint: f.otherKey},
		}}, true
	}
	now := time.Now().UTC()
	return ClusterMemberDiagnosticsRequest{DataDir: root, WorkDir: t.TempDir(), Request: diagnostics.BundleRequest{RequestedBy: "support-bundle", From: now.Add(-time.Hour), To: now.Add(time.Minute)}}
}

func collectedIndex(t *testing.T, req ClusterMemberDiagnosticsRequest) (map[string]diagnostics.MemberIndexEntry, map[string]string, diagnostics.Bundle) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	bundle, archive, err := CollectClusterMemberDiagnostics(ctx, req, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || bundle.State != "ready" {
		t.Fatalf("collection = %q, %v", bundle.State, err)
	}
	files := archiveFiles(t, archive)
	var index diagnostics.MemberIndex
	if err := json.Unmarshal([]byte(files["members/index.json"]), &index); err != nil {
		t.Fatal(err)
	}
	if index.Complete {
		t.Fatalf("member 11 does not answer, yet the collection says complete: %+v", index)
	}
	out := map[string]diagnostics.MemberIndexEntry{}
	for _, member := range index.Members {
		out[member.Address] = member
	}
	return out, files, bundle
}

// One member collects another's part over the real member route, with no
// database on either side; the third member is down.
func TestAMemberCollectsAnotherMembersPartOverTheMemberRoute(t *testing.T) {
	f := newClosedFactsTLSFixture(t)
	server, _ := diagnosticsMember(t, f, time.Now().UTC())
	// Finding the members takes this side 1.5 s; the two members share one
	// clock, so the offset must come out near zero all the same.
	members, files, _ := collectedIndex(t, collectingMember(t, f, "https://"+server.Addr(), f.serverKey, 1500*time.Millisecond))
	got := members["127.0.0.1:9910"]
	if got.Missing != "" || got.AnsweredKey != f.serverKey || got.ClockOffsetMs == nil || got.RoundTripMs == nil {
		t.Fatalf("member 7 = %+v", got)
	}
	if offset := *got.ClockOffsetMs; offset < -*got.RoundTripMs-50 || offset > *got.RoundTripMs+50 {
		t.Fatalf("clock offset = %d ms with a round trip of %d ms: the two members share one clock", offset, *got.RoundTripMs)
	}
	if log := files["members/127.0.0.1_9910/server.log"]; !strings.Contains(log, "event=step_down") {
		t.Fatalf("member 7's log:\n%s", log)
	}
	if !strings.Contains(files["members/127.0.0.1_9910/role-events.log"], "reason=no_majority_contact") || !strings.Contains(files["members/127.0.0.1_9910/leader-changes.log"], "old_leader=7 new_leader=9") {
		t.Fatal("member 7's role records are not in the collection")
	}
	if down := members["127.0.0.1:9912"]; down.Missing != "unreachable" || down.Evidence == "" {
		t.Fatalf("member 11 = %+v, want unreachable", down)
	}
	if self := members["127.0.0.1:9911"]; !self.Self || self.Missing != "" {
		t.Fatalf("own entry = %+v", self)
	}
}

// Where the cluster names the member's key, another member's certificate
// answering at that address is not taken for it.
func TestAPartFromAnotherMembersKeyIsNotTaken(t *testing.T) {
	f := newClosedFactsTLSFixture(t)
	server, _ := diagnosticsMember(t, f, time.Now().UTC())
	members, files, _ := collectedIndex(t, collectingMember(t, f, "https://"+server.Addr(), f.otherKey, 0))
	got := members["127.0.0.1:9910"]
	if got.Missing != "failed" || !strings.Contains(got.Evidence, "answered with member key "+f.serverKey) {
		t.Fatalf("member 7 = %+v", got)
	}
	if _, ok := files["members/127.0.0.1_9910/server.log"]; ok {
		t.Fatal("the part of the wrong key is in the collection")
	}
}

// A collection lists its members, asks each of them and writes its own
// identity and status from one observation of the cluster, and a member
// gives its part from one: with no leader each observation costs its
// probes' timeouts.
func TestACollectionObservesTheClusterOncePerMember(t *testing.T) {
	f := newClosedFactsTLSFixture(t)
	server, _ := diagnosticsMember(t, f, time.Now().UTC())
	req := collectingMember(t, f, "https://"+server.Addr(), f.serverKey, 0)
	inner := statusQuorumFetch
	var mu sync.Mutex
	observed := map[uint64]int{}
	statusQuorumFetch = func(ctx context.Context, s clusterStatus, timeout time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		mu.Lock()
		observed[s.id]++
		mu.Unlock()
		return inner(ctx, s, timeout)
	}
	members, files, _ := collectedIndex(t, req)
	if members["127.0.0.1:9910"].Missing != "" {
		t.Fatalf("member 7 = %+v", members["127.0.0.1:9910"])
	}
	mu.Lock()
	defer mu.Unlock()
	if observed[9] != 1 || observed[7] != 1 {
		t.Fatalf("observations per member = %v, want one by the collecting member 9 and one by the answering member 7", observed)
	}
	for _, name := range []string{"127.0.0.1_9910", "127.0.0.1_9911"} {
		status := files["members/"+name+"/cluster-status.txt"]
		if !strings.HasPrefix(status, "Observed: 20") || !strings.Contains(status, "\nMember: ") {
			t.Fatalf("%s status does not say when it was observed:\n%.200s", name, status)
		}
	}
}
