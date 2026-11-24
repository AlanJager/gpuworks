//go:build linux && dqlite

package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Who closed admission is logged at the default level at the first, second
// and fourth close of a closed period; failed proofs likewise, with their
// stage and duration, only while admission is closed; one line ends the
// period with both counts, and the next period starts its lines again.
func TestAdmissionJournalNamesTheCloserAndTheFailedProofs(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	var j admissionJournal
	// A failed proof of a member that still serves is not this journal's.
	j.proof(log, false, "deadline_exceeded", "membership_grant", "other", 5*time.Second)
	j.proof(log, true, "none", "", "", time.Second)
	if out.Len() != 0 {
		t.Fatalf("an open member's proofs logged:\n%s", out.String())
	}
	for range 5 {
		j.closed(log, "app.(*App).replicateMaterialCycle:39")
	}
	text := out.String()
	if strings.Count(text, `msg="closed business admission"`) != 3 || strings.Count(text, "level=INFO") != 3 {
		t.Fatalf("5 closes logged:\n%s", text)
	}
	for _, want := range []string{"caller=app.(*App).replicateMaterialCycle:39 closes=1", "closes=2", "closes=4"} {
		if !strings.Contains(text, want) {
			t.Fatalf("want %q in:\n%s", want, text)
		}
	}
	out.Reset()
	for range 3 {
		j.proof(log, false, "deadline_exceeded", "remote_quorum", "other", 15300*time.Millisecond)
	}
	text = out.String()
	if strings.Count(text, "candidate material proof failed while business admission is closed") != 2 {
		t.Fatalf("3 failed proofs logged:\n%s", text)
	}
	for _, want := range []string{"failed_proofs=1 err_class=deadline_exceeded failure_stage=remote_quorum failure_class=other total_ms=15300", "failed_proofs=2", "closed_ms="} {
		if !strings.Contains(text, want) {
			t.Fatalf("want %q in:\n%s", want, text)
		}
	}
	out.Reset()
	j.proof(log, true, "none", "", "", 2*time.Second)
	j.proof(log, true, "none", "", "", 2*time.Second)
	text = out.String()
	if strings.Count(text, "candidate material proof succeeded after business admission closed") != 1 || !strings.Contains(text, "failed_proofs=3 closes=5 closed_ms=") {
		t.Fatalf("the period ended with:\n%s", text)
	}
	out.Reset()
	j.closed(log, "app.(*materialEligibility).Reprove:412")
	if text = out.String(); !strings.Contains(text, "caller=app.(*materialEligibility).Reprove:412 closes=1") {
		t.Fatalf("the next period began with:\n%s", text)
	}
}

// The caller is the function that asked, not the closing helpers.
func TestClosingAdmissionLogsTheFunctionThatAsked(t *testing.T) {
	var out bytes.Buffer
	a := &App{log: slog.New(slog.NewTextHandler(&out, nil))}
	a.invalidateCandidateMaterialEligibility()
	a.closeBusinessAdmission()
	text := out.String()
	if strings.Count(text, "caller=app.TestClosingAdmissionLogsTheFunctionThatAsked:") != 2 || !strings.Contains(text, "closes=1") || !strings.Contains(text, "closes=2") {
		t.Fatalf("closes logged:\n%s", text)
	}
	if strings.Contains(text, "invalidateCandidateMaterialEligibility") || strings.Contains(text, "closeBusinessAdmission:") {
		t.Fatalf("a closing helper was named as the caller:\n%s", text)
	}
}
