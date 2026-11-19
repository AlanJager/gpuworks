package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The server fails an instance only with a reason: SetInstanceState refuses
// the failed state, SetInstanceFailed refuses an empty code, and a failure
// it records is stored whole and goes on the timeline event next to the
// detail. A later transition clears it.
func TestSetInstanceFailedRequiresAndRecordsItsReason(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateInstance(&Instance{UUID: "server-failed", DeploymentID: 3, State: proto.InstPending}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInstanceState("server-failed", proto.InstFailed, "no reason"); !errors.Is(err, ErrFailedNeedsReason) {
		t.Fatalf("SetInstanceState(failed) = %v", err)
	}
	if err := s.SetInstanceFailed("server-failed", proto.RuntimeFailure{}, "no code"); !errors.Is(err, ErrFailedNeedsReason) {
		t.Fatalf("SetInstanceFailed(no code) = %v", err)
	}
	if in, _ := s.InstanceByUUID("server-failed"); in.State != proto.InstPending {
		t.Fatalf("a refused failure was written: %+v", in)
	}
	rank := 1
	if err := s.SetInstanceFailed("server-failed", proto.RuntimeFailure{Code: proto.ErrorUnitRankNodeOffline, Rank: &rank, Node: "gpu-b", Detail: strings.Repeat("x", 1000)}, "rank 1 on gpu-b failed"); err != nil {
		t.Fatal(err)
	}
	in, _ := s.InstanceByUUID("server-failed")
	f := in.Failure()
	if in.State != proto.InstFailed || in.Message != "rank 1 on gpu-b failed" || f == nil || f.Code != proto.ErrorUnitRankNodeOffline || *f.Rank != 1 || f.Node != "gpu-b" {
		t.Fatalf("row = %+v reason %+v", in, f)
	}
	if len(f.Detail) > proto.MaxFailureEvidenceChars {
		t.Fatalf("stored detail is unbounded: %d bytes", len(f.Detail))
	}
	evs, err := s.InstanceTimeline("server-failed", 0, 10)
	if err != nil || len(evs) == 0 {
		t.Fatalf("timeline = %+v, %v", evs, err)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(evs[0].Data), &params); err != nil {
		t.Fatal(err)
	}
	if ev := EventFailure(params); params["detail"] != "rank 1 on gpu-b failed" || ev == nil || ev.Code != proto.ErrorUnitRankNodeOffline || ev.Node != "gpu-b" {
		t.Fatalf("event params = %v", params)
	}
	if err := s.SetInstanceState("server-failed", proto.InstRemoving, "retry requested"); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.InstanceByUUID("server-failed"); in.RuntimeFailure != "" || in.Failure() != nil {
		t.Fatalf("a later transition kept the reason: %+v", in)
	}
	evs, _ = s.InstanceTimeline("server-failed", 0, 1)
	params = nil
	_ = json.Unmarshal([]byte(evs[0].Data), &params)
	if _, ok := params[eventFailureParam]; ok {
		t.Fatalf("a transition without a failure carries one: %v", params)
	}
}

// A row the server fails was last seen in some progress phase of its own
// start (a leader still unpacking when a follower's container exits). That
// phase is not where the failure happened, so it does not stay on the failed
// row (GWKS-342); the failure carries its own phase. A failed sleep keeps
// its phase, which is what holds its GPU claim.
func TestServerFailureDoesNotKeepTheStartsProgressPhase(t *testing.T) {
	s := newTestStore(t)
	for _, uuid := range []string{"leader", "asleep"} {
		if _, err := s.CreateInstance(&Instance{UUID: uuid, DeploymentID: 3, State: proto.InstPending}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: "leader", State: proto.InstStarting, Phase: proto.PhaseContainerUnpack,
		Message: "unpacking image layers into container root filesystem", Progress: 0.4, BytesDone: 40, BytesTotal: 100, RateBPS: 7}); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.InstanceByUUID("leader"); in.Phase != proto.PhaseContainerUnpack || in.BytesDone != 40 {
		t.Fatalf("progress was not stored: %+v", in)
	}
	rank, code := 1, 1
	follower := proto.RuntimeFailure{Code: proto.ErrorContainerExited, Phase: proto.PhaseHealthCheck, ExitCode: &code, Rank: &rank, Node: "gpu-b",
		Evidence: []string{"This recipe requires TP16 across two nodes, with eight NPUs per node."}}
	if err := s.SetInstanceFailed("leader", follower, "rank 1 on gpu-b failed: headless worker exited (code 1): This recipe requires TP16 across two nodes, with eight NPUs per node."); err != nil {
		t.Fatal(err)
	}
	in, _ := s.InstanceByUUID("leader")
	if in.State != proto.InstFailed || in.Phase != "" || in.Progress != 0 || in.BytesDone != 0 || in.BytesTotal != 0 || in.RateBPS != 0 {
		t.Fatalf("failed row keeps the start's progress: phase %q progress %v bytes %d/%d rate %d", in.Phase, in.Progress, in.BytesDone, in.BytesTotal, in.RateBPS)
	}
	if f := in.Failure(); f == nil || f.Phase != proto.PhaseHealthCheck || len(f.Evidence) != 1 {
		t.Fatalf("failure = %+v", f)
	}

	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: "asleep", State: proto.InstRunning, Phase: proto.PhaseSleep, SleepLevel: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInstanceFailed("asleep", proto.RuntimeFailure{Code: proto.ErrorUnitRankNodeOffline}, "node offline beyond reconnect grace"); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.InstanceByUUID("asleep"); in.State != proto.InstFailed || in.Phase != proto.PhaseSleep {
		t.Fatalf("a failed sleep lost its phase: state %q phase %q", in.State, in.Phase)
	}
}

// An agent's report is stored in the one spelling and bounded again: an
// older agent's lower-case code, or one that sends more evidence than the
// wire allows, is read the way a current one is.
func TestReportedRuntimeFailureIsNormalizedAndBounded(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateInstance(&Instance{UUID: "reported", State: proto.InstStarting}); err != nil {
		t.Fatal(err)
	}
	failure := &proto.RuntimeFailure{Code: "engine_port_refused", Port: 30000, Evidence: []string{"a", "b", "c", "d", "e"}}
	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: "reported", State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED: port 30000", RuntimeFailure: failure}); err != nil {
		t.Fatal(err)
	}
	in, _ := s.InstanceByUUID("reported")
	f := in.Failure()
	if f == nil || f.Code != proto.ErrorEnginePortRefused || f.Port != 30000 || len(f.Evidence) != proto.MaxFailureEvidence || f.Evidence[0] != "c" {
		t.Fatalf("stored reason = %+v (%s)", f, in.RuntimeFailure)
	}
	if !strings.Contains(in.RuntimeFailure, `"code":"ENGINE_PORT_REFUSED"`) {
		t.Fatalf("column = %s", in.RuntimeFailure)
	}
	// A row written by an older server keeps its lower-case code until read.
	old := Instance{RuntimeFailure: `{"code":"container_exited","exit_code":1}`}
	if f := old.Failure(); f == nil || f.Code != proto.ErrorContainerExited || *f.ExitCode != 1 {
		t.Fatalf("old row reason = %+v", f)
	}
	for _, malformed := range []string{"", "{}", "not json", `{"code":""}`} {
		if (&Instance{RuntimeFailure: malformed}).Failure() != nil {
			t.Fatalf("%q read as a reason", malformed)
		}
	}
}

// An agent that predates RuntimeFailure named a port refusal only as its
// message's prefix. Its failed row reads as the coded reason, with the port;
// nothing else is read from a message: not another prefix, not the refusal
// text elsewhere in it, not a row that is not failed, and never over a
// stored reason.
func TestOldAgentPortRefusalReadsAsItsCode(t *testing.T) {
	old := Instance{State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED: port 30007 is already in use on this node; nothing was started"}
	f := old.Failure()
	if f == nil || f.Code != proto.ErrorEnginePortRefused || f.Port != 30007 || f.Phase != proto.PhaseContainerPrepare {
		t.Fatalf("old refusal = %+v", f)
	}
	for name, in := range map[string]Instance{
		"in use":        {State: proto.InstFailed, Message: "ENGINE_PORT_IN_USE: port 30000 is already in use on this node"},
		"nested":        {State: proto.InstFailed, Message: "rank 1 on n failed: ENGINE_PORT_REFUSED: port 30000"},
		"bare code":     {State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED"},
		"other failure": {State: proto.InstFailed, Message: "image pull timed out"},
		"not failed":    {State: proto.InstRemoving, Message: "ENGINE_PORT_REFUSED: port 30000 is already in use"},
		"empty":         {State: proto.InstFailed},
	} {
		if f := in.Failure(); f != nil {
			t.Errorf("%s: read as %+v", name, f)
		}
	}
	if f := (&Instance{State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED: port 1 x"}).Failure(); f == nil || f.Port != 1 {
		t.Fatalf("short refusal = %+v", f)
	}
	stored := Instance{State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED: port 30000 x", RuntimeFailure: `{"code":"CONTAINER_EXITED"}`}
	if f := stored.Failure(); f == nil || f.Code != proto.ErrorContainerExited {
		t.Fatalf("stored reason was overridden by the message: %+v", f)
	}
	unparsable := Instance{State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED: port many are taken"}
	if f := unparsable.Failure(); f == nil || f.Port != 0 {
		t.Fatalf("unparsable port = %+v", f)
	}
}

// The phase a server-recorded failure ends is cleared from the row, so the
// report that later ends the row has nothing left to summarize: the summary
// of the ended phase is on the failure's own event.
func TestServerFailureEventSummarizesThePhaseItEnds(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateInstance(&Instance{UUID: "leader", DeploymentID: 3, State: proto.InstPending}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: "leader", State: proto.InstStarting, Phase: proto.PhaseContainerUnpack, Progress: 0.4, BytesDone: 40, BytesTotal: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInstanceFailed("leader", proto.RuntimeFailure{Code: proto.ErrorContainerExited}, "rank 1 failed"); err != nil {
		t.Fatal(err)
	}
	events, err := s.InstanceEventsByUUID("leader", 10)
	if err != nil {
		t.Fatal(err)
	}
	var failed map[string]any
	for _, ev := range events {
		if ev.Code == instanceEventStateChanged {
			if err := json.Unmarshal([]byte(ev.Data), &failed); err != nil {
				t.Fatal(err)
			}
		}
	}
	if failed["prev_phase"] != proto.PhaseContainerUnpack || failed["prev_state"] != proto.InstStarting || failed["prev_bytes_done"] != float64(40) || failed["prev_bytes_total"] != float64(100) {
		t.Fatalf("failure event does not summarize the phase it ends: %v", failed)
	}
}

// The failure decides whether to clear the phase on the row as it is when it
// is written. An instance read mid-start and reported asleep before the write
// keeps its sleep phase, which is what holds its GPU claim.
func TestServerFailureKeepsASleepReportedAfterTheRowWasRead(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateInstance(&Instance{UUID: "racing", DeploymentID: 3, State: proto.InstPending}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: "racing", State: proto.InstRunning, Phase: proto.PhaseHealthCheck}); err != nil {
		t.Fatal(err)
	}
	// The sleep lands after setInstanceState has read the row and before its
	// first write: on the write's own connection, just ahead of it.
	landed := false
	const hook = "test:sleep-lands-before-failure"
	if err := s.db.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
		if landed || tx.Statement.Table != "instances" {
			return
		}
		landed = true
		if err := tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE instances SET phase = ? WHERE uuid = ?", proto.PhaseSleep, "racing").Error; err != nil {
			t.Errorf("land the sleep: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.db.Callback().Update().Remove(hook) })
	if err := s.SetInstanceFailed("racing", proto.RuntimeFailure{Code: proto.ErrorUnitRankNodeOffline}, "node offline beyond reconnect grace"); err != nil {
		t.Fatal(err)
	}
	if !landed {
		t.Fatal("the sleep never landed; the case is void")
	}
	if in, _ := s.InstanceByUUID("racing"); in.State != proto.InstFailed || in.Phase != proto.PhaseSleep {
		t.Fatalf("state %q phase %q, want failed and still asleep", in.State, in.Phase)
	}
}
