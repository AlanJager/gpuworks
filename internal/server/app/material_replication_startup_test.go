//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"syscall"
	"testing"
	"time"
)

func TestMaterialStartupFailurePreservesErrorAndAuthority(t *testing.T) {
	live := errors.New("independent material integrity failure")
	unavailable := &url.Error{Op: "Post", URL: "https://fixture.invalid", Err: syscall.ECONNREFUSED}
	for _, mode := range []string{"nil", "live", "nested-live", "unavailable", "pure-cancel", "joined-pure-cancel", "joined-live-cancel", "unavailable-cancel", "callback-cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var input error
			switch mode {
			case "live", "callback-cancel":
				input = live
			case "nested-live":
				input = fmt.Errorf("outer: %w", errors.Join(context.Canceled, fmt.Errorf("inner: %w", live)))
			case "unavailable", "unavailable-cancel":
				input = unavailable
			case "pure-cancel":
				input = context.Canceled
			case "joined-pure-cancel":
				input = fmt.Errorf("outer: %w", errors.Join(context.Canceled, context.DeadlineExceeded))
			case "joined-live-cancel":
				input = errors.Join(live, context.Canceled)
			}
			canceled := mode == "pure-cancel" || mode == "joined-pure-cancel" || mode == "joined-live-cancel" || mode == "unavailable-cancel" || mode == "callback-cancel"
			if canceled && mode != "callback-cancel" {
				cancel()
			}
			invalidated := 0
			gate := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, materialReproofInterval)
			gate.setOnInvalidated(func() {
				invalidated++
				if mode == "callback-cancel" {
					cancel()
				}
			})
			if ready, err := gate.Reprove(t.Context()); !ready || err != nil {
				t.Fatal(ready, err)
			}
			a := &App{memberState: memberState{materialEligibility: gate}}
			err, stop := a.materialStartupFailure(ctx, input)
			if stop != canceled {
				t.Fatalf("stop=%v want%v err%v", stop, canceled, err)
			}
			wantInvalidation := mode == "live" || mode == "nested-live" || mode == "joined-live-cancel" || mode == "callback-cancel"
			if wantInvalidation {
				if invalidated != 1 || gate.Eligible() {
					t.Fatal("live veto did not close old proof", invalidated)
				}
			} else if invalidated != 0 || !gate.Eligible() {
				t.Fatal("noninvalidating result changed old proof", invalidated)
			}
			if canceled && !errors.Is(err, context.Canceled) {
				t.Fatal("lost cancellation", err)
			}
			if mode == "pure-cancel" || mode == "joined-pure-cancel" {
				if err != context.Canceled {
					t.Fatal("pure cancellation identity changed", err)
				}
			}
			if wantInvalidation && !errors.Is(err, live) {
				t.Fatal("lost independent live cause", err)
			}
			if mode == "unavailable-cancel" && !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatal("lost independent availability cause", err)
			}
			if (mode == "nil" || mode == "unavailable") && err != nil {
				t.Fatal("ordinary fallback rejected", err)
			}
		})
	}
}

func TestMaterialStartupResultPreservesOriginalClassification(t *testing.T) {
	live := errors.New("live material veto")
	unavailable := &url.Error{Op: "Post", URL: "https://fixture.invalid", Err: syscall.ECONNREFUSED}
	for _, mode := range []string{"nil", "canceled", "deadline", "unavailable", "live"} {
		t.Run(mode, func(t *testing.T) {
			var original error
			switch mode {
			case "canceled":
				original = context.Canceled
			case "deadline":
				original = context.DeadlineExceeded
			case "unavailable":
				original = unavailable
			case "live":
				original = live
			}
			result := materialStartupResult(nil, original)
			if result != original {
				t.Fatalf("no veto changed original identity: original=%v result=%v", original, result)
			}
			if materialReplicationFailureInvalidates(result) != materialReplicationFailureInvalidates(original) {
				t.Fatalf("no veto changed original classification: original=%v result=%v", original, result)
			}
		})
	}

	canceled := materialStartupResult(live, context.Canceled)
	if !errors.Is(canceled, live) || !errors.Is(canceled, context.Canceled) {
		t.Fatal("veto or cancellation lost", canceled)
	}
	if !materialReplicationFailureInvalidates(canceled) {
		t.Fatal("live veto became noninvalidating")
	}
	if result := materialStartupResult(live, nil); !errors.Is(result, live) {
		t.Fatal("successful repair erased veto", result)
	}
}
