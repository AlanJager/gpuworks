//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Every 503 of the manifest receiver goes through one refusal. Only a check
// that was interrupted before it reached a verdict is labelled; a finding
// about bytes, identity, pointer or membership never is.
func TestManifestReceiverLabelsOnlyChecksThatReachedNoVerdict(t *testing.T) {
	mismatch := errors.New("config material differs locally: digest mismatch")
	noLeader := fmt.Errorf("grant: %w", dqlitedriver.ErrNoAvailableLeader)
	for _, tc := range []struct {
		name   string
		reason materialManifestRefusal
		cause  error
		body   string
		label  string
	}{
		{"not current, mismatch", manifestNotCurrent, mismatch, "material manifest is not current or complete\n", ""},
		{"not current: no leader to read it from", manifestNotCurrent, noLeader, "material manifest is not current or complete\n", materialRefusalInterrupted},
		{"not current: mismatch joined to a lost leader", manifestNotCurrent, errors.Join(noLeader, mismatch), "material manifest is not current or complete\n", ""},
		{"not current: a material file is missing", manifestNotCurrent, &fs.PathError{Op: "open", Path: "material/blob", Err: syscall.ENOENT}, "material manifest is not current or complete\n", ""},
		{"not current: a material file cannot be read", manifestNotCurrent, &fs.PathError{Op: "open", Path: "material/blob", Err: syscall.EACCES}, "material manifest is not current or complete\n", ""},
		{"not current: dropped connection", manifestNotCurrent, driver.ErrBadConn, "material manifest is not current or complete\n", ""},
		{"changed: a file went missing", manifestChangedInstalled, &fs.PathError{Op: "open", Path: "material/blob", Err: syscall.ENOENT}, "material manifest changed during installed check\n", ""},
		{"references not portable", manifestReferencesNotPortable, mismatch, "material consumer references are not portable\n", ""},
		{"install failed", manifestInstallFailed, mismatch, "material consumer paths could not be installed\n", ""},
		{"not installed", manifestNotInstalled, mismatch, "material consumer paths are not installed\n", ""},
		{"interrupted stream", manifestInterrupted, context.Canceled, "material verification was interrupted\n", materialRefusalInterrupted},
		{"interrupted, no cause recorded", manifestInterrupted, nil, "material verification was interrupted\n", materialRefusalInterrupted},
		{"changed: bytes differ", manifestChangedInstalled, mismatch, "material manifest changed during installed check\n", ""},
		{"changed: pointer row gone", manifestChangedInstalled, gorm.ErrRecordNotFound, "material manifest changed during installed check\n", ""},
		{"changed: requester revoked", manifestChangedInstalled, store.ErrClusterAdmissionRejected, "material manifest changed during installed check\n", ""},
		{"changed: revoked joined with a dropped connection", manifestChangedInstalled, errors.Join(store.ErrClusterAdmissionRejected, driver.ErrBadConn), "material manifest changed during installed check\n", ""},
		{"changed: mismatch joined with no leader", manifestChangedInstalled, errors.Join(mismatch, noLeader), "material manifest changed during installed check\n", ""},
		{"changed: no cause recorded", manifestChangedInstalled, nil, "material manifest changed during installed check\n", ""},
		{"changed: no leader", manifestChangedInstalled, noLeader, "material manifest changed during installed check\n", materialRefusalInterrupted},
		{"changed: dropped connection", manifestChangedInstalled, driver.ErrBadConn, "material manifest changed during installed check\n", materialRefusalInterrupted},
		{"changed: connection closed", manifestChangedInstalled, io.ErrUnexpectedEOF, "material manifest changed during installed check\n", materialRefusalInterrupted},
		{"changed: deadline", manifestChangedInstalled, fmt.Errorf("grant: %w", context.DeadlineExceeded), "material manifest changed during installed check\n", materialRefusalInterrupted},
		{"changed during installation: no leader", manifestChangedInstalling, noLeader, "material manifest changed during installation\n", materialRefusalInterrupted},
		{"changed during installation: bytes differ", manifestChangedInstalling, mismatch, "material manifest changed during installation\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			w.Header().Set("X-Gpuworks-Material-Digest", "staged")
			refuseMaterialManifest(w, tc.reason, tc.cause)
			response := w.Result()
			if response.StatusCode != http.StatusServiceUnavailable || w.Body.String() != tc.body {
				t.Fatalf("answer = %d %q, want 503 %q", response.StatusCode, w.Body.String(), tc.body)
			}
			if got := response.Header.Get(materialRefusalHeader); got != tc.label {
				t.Fatalf("refusal label = %q, want %q", got, tc.label)
			}
			if response.Header.Get("X-Gpuworks-Material-Digest") != "" {
				t.Fatal("a refusal kept the staged receipt binding")
			}
			// The sender: a labelled refusal is an unavailable peer, never a
			// verified copy and never a verdict that closes its business.
			refusal := fmt.Errorf("check member 2: %w", newMaterialReceiverStatus(response, "manifest receiver rejected current bytes or identity: HTTP 503"))
			if got, want := peerUnreachable(refusal), tc.label != ""; got != want {
				t.Fatalf("sender takes the peer as unavailable = %v, want %v", got, want)
			}
			if got, want := materialReplicationFailureInvalidates(refusal), tc.label == ""; got != want {
				t.Fatalf("sender closes on it = %v, want %v", got, want)
			}
		})
	}
}

// The label excuses a 503 only, and only the exact value.
func TestManifestInterruptedLabelIsAnUnavailablePeerOnlyOn503(t *testing.T) {
	for _, tc := range []struct {
		status      int
		label       string
		unavailable bool
	}{
		{http.StatusServiceUnavailable, materialRefusalInterrupted, true},
		{http.StatusServiceUnavailable, materialRefusalInterrupted + "-more", false},
		{http.StatusForbidden, materialRefusalInterrupted, false},
		{http.StatusNoContent, materialRefusalInterrupted, false},
	} {
		response := statusResponse(tc.status)
		response.Header.Set(materialRefusalHeader, tc.label)
		err := fmt.Errorf("check member 2: %w", newMaterialReceiverStatus(response, "manifest receiver rejected current bytes or identity"))
		if got := peerUnreachable(err); got != tc.unavailable {
			t.Errorf("HTTP %d label %q: unavailable peer = %v, want %v", tc.status, tc.label, got, tc.unavailable)
		}
	}
}

// The final receipt confirms the installed bytes once more. An interrupted
// confirmation is labelled; a confirmation that found a difference is not.
func TestManifestReceiptLabelsAnInterruptedConfirmationOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		confirm error
		cancel  bool
		label   string
	}{
		{"bytes changed", errors.New("installed bytes differ"), false, ""},
		{"leader lost during confirmation", fmt.Errorf("grant: %w", dqlitedriver.ErrNoAvailableLeader), false, materialRefusalInterrupted},
		{"request cancelled", nil, true, materialRefusalInterrupted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := newMaterialRequestTracker()
			t.Cleanup(requests.closeAdmissions)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := httptest.NewRequest(http.MethodPost, "/installed", nil).WithContext(ctx)
			w := httptest.NewRecorder()
			writeMaterialManifestReceipt(w, r, requests, 1, 2, materialTestDigest([]byte("manifest")), func() error {
				if tc.cancel {
					cancel()
				}
				return tc.confirm
			}, manifestChangedInstalled)
			response := w.Result()
			if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get(materialRefusalHeader) != tc.label {
				t.Fatalf("receipt = %d label %q, want 503 label %q", response.StatusCode, response.Header.Get(materialRefusalHeader), tc.label)
			}
			if response.Header.Get("X-Gpuworks-Material-Source") != "" {
				t.Fatal("a refused receipt kept its binding")
			}
		})
	}
}

// What closed a restarted member in the field: its restart check asked a
// peer whose own check was interrupted by a database leadership change. That
// answer alone must fall back to full replication without closing the
// business a fresh proof opened. An unlabelled answer, or a second cause
// joined to the labelled one, keeps its verdict.
func TestRestartCheckDoesNotCloseOnAnInterruptedPeerCheck(t *testing.T) {
	answer := func(label string) error {
		response := statusResponse(http.StatusServiceUnavailable)
		if label != "" {
			response.Header.Set(materialRefusalHeader, label)
		}
		// The restart check wraps the receiver's answer with what it learned
		// about the peer's database port.
		return fmt.Errorf("%w; database 172.20.9.30:9001 accepted a TCP connection",
			newMaterialReceiverStatus(response, "manifest receiver rejected current bytes or identity: HTTP 503"))
	}
	for _, tc := range []struct {
		name   string
		input  error
		closes bool
	}{
		{"peer check interrupted", answer(materialRefusalInterrupted), false},
		{"peer reports changed bytes", answer(""), true},
		{"interrupted peer joined with a local mismatch", errors.Join(answer(materialRefusalInterrupted), errors.New("local master secret differs")), true},
		{"interrupted peer joined with an unlabelled peer", errors.Join(answer(materialRefusalInterrupted), answer("")), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalidated := 0
			gate := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, materialReproofInterval)
			gate.setOnInvalidated(func() { invalidated++ })
			if ready, err := gate.Reprove(t.Context()); !ready || err != nil {
				t.Fatal(ready, err)
			}
			a := &App{memberState: memberState{materialEligibility: gate}}
			rejected, stop := a.materialStartupFailure(t.Context(), tc.input)
			if stop {
				t.Fatal("the full replication that follows was stopped")
			}
			if closed := invalidated != 0 || !gate.Eligible(); closed != tc.closes {
				t.Fatalf("business closed = %v (invalidations %d), want %v", closed, invalidated, tc.closes)
			}
			if (rejected != nil) != tc.closes {
				t.Fatalf("carried veto = %v, want one only when it closes", rejected)
			}
		})
	}
}
