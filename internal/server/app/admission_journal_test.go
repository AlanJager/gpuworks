//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func journalledApp(t *testing.T, measure func(context.Context) (bool, CandidateProofMetrics, error)) (*App, *materialEligibility, *materialTestClock, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(out, nil))
	a := &App{log: log}
	clock := &materialTestClock{now: time.Unix(100, 0)}
	eligibility := newJournalledMaterialEligibility(a, log, clock.Now, measure)
	eligibility.setOnInvalidated(a.closeBusinessAdmission)
	a.materialEligibility = eligibility
	return a, eligibility, clock, out
}

func count(text, line string) int { return strings.Count(text, `msg="`+line+`"`) }

const (
	journalClosed    = "closed business admission"
	journalFailed    = "candidate material proof failed while business admission is closed"
	journalRecovered = "candidate material proof succeeded after business admission closed"
)

// Through the proof worker as the member wires it: an open member's proofs
// leave nothing; one invalidation leaves one line that names where it came
// from; the proofs that fail while closed leave their stage and duration at
// the first, second and fourth; an accepted proof ends the period once.
func TestAdmissionJournalThroughTheProofWorker(t *testing.T) {
	ready, stage, fail := true, "", error(nil)
	a, eligibility, _, out := journalledApp(t, func(context.Context) (bool, CandidateProofMetrics, error) {
		return ready, CandidateProofMetrics{FailureStage: stage, FailureClass: "other", Total: CandidateProofStage{Calls: 1, Duration: 1500 * time.Millisecond}}, fail
	})
	if ok, err := eligibility.Reprove(t.Context()); !ok || err != nil {
		t.Fatalf("initial proof: %t %v", ok, err)
	}
	if ok, err := eligibility.Reprove(t.Context()); !ok || err != nil || out.Len() != 0 {
		t.Fatalf("an open member's proofs logged (ready=%t err=%v):\n%s", ok, err, out.String())
	}
	// One invalidation reaches the close twice (the worker's callback, then
	// directly); the same place closing again says nothing new either.
	for range 4 {
		a.invalidateCandidateMaterialEligibility()
	}
	text := out.String()
	if count(text, journalClosed) != 1 || !strings.Contains(text, "caller=app.TestAdmissionJournalThroughTheProofWorker:") || !strings.Contains(text, "first_of_period=true") {
		t.Fatalf("four invalidations from one place logged:\n%s", text)
	}
	out.Reset()
	ready, stage = false, "received_manifest"
	for range 5 {
		if ok, _ := eligibility.Reprove(t.Context()); ok {
			t.Fatal("a failed proof reported ready")
		}
	}
	text = out.String()
	if count(text, journalFailed) != 3 || count(text, journalClosed) != 0 {
		t.Fatalf("5 failed proofs of a closed member logged:\n%s", text)
	}
	for _, want := range []string{"failed_proofs=1 err_class=none failure_stage=received_manifest failure_class=other total_ms=1500", "failed_proofs=2", "failed_proofs=4", "closed_ms="} {
		if !strings.Contains(text, want) {
			t.Fatalf("want %q in:\n%s", want, text)
		}
	}
	out.Reset()
	ready, stage = true, ""
	for range 2 {
		if ok, err := eligibility.Reprove(t.Context()); !ok || err != nil {
			t.Fatalf("recovering proof: %t %v", ok, err)
		}
	}
	text = out.String()
	if count(text, journalRecovered) != 1 || !strings.Contains(text, "failed_proofs=5 close_callers=1 closed_ms=") || !eligibility.Eligible() {
		t.Fatalf("the period ended with (eligible=%t):\n%s", eligibility.Eligible(), text)
	}
}

// The proof whose own answer closes the member is the first failed proof of
// the period, with its stage; the close names the proof worker.
func TestAdmissionJournalHasTheProofThatClosedTheMember(t *testing.T) {
	ready, fail := true, error(nil)
	_, eligibility, clock, out := journalledApp(t, func(context.Context) (bool, CandidateProofMetrics, error) {
		stage := ""
		if fail != nil {
			stage = "local_hash"
		}
		return ready, CandidateProofMetrics{FailureStage: stage, FailureClass: "other", Total: CandidateProofStage{Calls: 1, Duration: 300 * time.Millisecond}}, fail
	})
	if ok, err := eligibility.Reprove(t.Context()); !ok || err != nil {
		t.Fatalf("initial proof: %t %v", ok, err)
	}
	// Past the proof's window an answer that is not success closes at once.
	clock.Advance(materialEligibilityWindow + time.Second)
	ready, fail = false, errors.New("installed material differs")
	if ok, _ := eligibility.Reprove(t.Context()); ok || eligibility.Eligible() {
		t.Fatal("the member stayed open")
	}
	text := out.String()
	if count(text, journalClosed) != 1 || !strings.Contains(text, "caller=app.(*materialEligibility).") {
		t.Fatalf("the close logged:\n%s", text)
	}
	if count(text, journalFailed) != 1 || !strings.Contains(text, "failed_proofs=1 err_class=other failure_stage=local_hash") {
		t.Fatalf("the closing proof logged:\n%s", text)
	}
	if strings.Index(text, journalClosed) > strings.Index(text, journalFailed) {
		t.Fatalf("the proof line came before the close:\n%s", text)
	}
	// Further failed proofs of the closed member add no close line.
	out.Reset()
	for range 3 {
		eligibility.Reprove(t.Context())
	}
	if text = out.String(); count(text, journalClosed) != 0 || count(text, journalFailed) != 2 {
		t.Fatalf("failed proofs of a closed member logged:\n%s", text)
	}
}

// A proof that succeeds while the member is invalidated under it, or returns
// after its window, is not a recovery: the member stays closed and the line
// says the result was not accepted.
func TestAdmissionJournalDoesNotCallAnUnacceptedProofARecovery(t *testing.T) {
	for _, mode := range []string{"invalidated during the proof", "returned after its window"} {
		t.Run(mode, func(t *testing.T) {
			var a *App
			var clock *materialTestClock
			disturb := false
			measure := func(context.Context) (bool, CandidateProofMetrics, error) {
				if disturb && mode == "invalidated during the proof" {
					a.invalidateCandidateMaterialEligibility()
				}
				if disturb && mode == "returned after its window" {
					clock.Advance(materialEligibilityWindow + time.Second)
				}
				return true, CandidateProofMetrics{Total: CandidateProofStage{Calls: 1, Duration: time.Second}}, nil
			}
			var eligibility *materialEligibility
			var out *bytes.Buffer
			a, eligibility, clock, out = journalledApp(t, measure)
			if ok, err := eligibility.Reprove(t.Context()); !ok || err != nil {
				t.Fatalf("initial proof: %t %v", ok, err)
			}
			a.invalidateCandidateMaterialEligibility()
			out.Reset()
			disturb = true
			if ok, _ := eligibility.Reprove(t.Context()); ok || eligibility.Eligible() {
				t.Fatal("an unaccepted proof opened the member")
			}
			text := out.String()
			if count(text, journalRecovered) != 0 || count(text, journalFailed) != 1 || !strings.Contains(text, "failure_stage=result_not_accepted") {
				t.Fatalf("an unaccepted proof logged:\n%s", text)
			}
			disturb = false
			out.Reset()
			if ok, err := eligibility.Reprove(t.Context()); !ok || err != nil || count(out.String(), journalRecovered) != 1 {
				t.Fatalf("the accepted proof (ready=%t err=%v) logged:\n%s", ok, err, out.String())
			}
		})
	}
}

// Closes from different places in one period each leave a line; the caller
// is where the close came from, never a closing helper.
func TestAdmissionJournalNamesEachPlaceAClosureCameFrom(t *testing.T) {
	a, eligibility, _, out := journalledApp(t, func(context.Context) (bool, CandidateProofMetrics, error) {
		return true, CandidateProofMetrics{}, nil
	})
	if ok, err := eligibility.Reprove(t.Context()); !ok || err != nil {
		t.Fatalf("initial proof: %t %v", ok, err)
	}
	a.invalidateCandidateMaterialEligibility()
	func() { a.closeBusinessAdmission() }()
	text := out.String()
	if count(text, journalClosed) != 2 || !strings.Contains(text, "first_of_period=true") || !strings.Contains(text, "first_of_period=false") {
		t.Fatalf("two places logged:\n%s", text)
	}
	if strings.Contains(text, "invalidateCandidateMaterialEligibility") || strings.Contains(text, "closeBusinessAdmission:") || strings.Contains(text, "Invalidate:") {
		t.Fatalf("a closing helper was named as the caller:\n%s", text)
	}
}
