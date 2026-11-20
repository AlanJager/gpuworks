//go:build linux && dqlite

package store

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

func assertClusterGrantDiagnostic(t *testing.T, output *bytes.Buffer, operation, phase string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry["msg"] == "cluster grant diagnostic" && entry["operation"] == operation {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s diagnostic, got %d: %s", operation, len(found), output.String())
	}
	entry := found[0]
	if entry["phase"] != phase {
		t.Fatalf("phase=%v want %s", entry["phase"], phase)
	}
	if elapsed, ok := entry["elapsed_ms"].(float64); !ok || elapsed < 0 {
		t.Fatalf("elapsed=%v", entry["elapsed_ms"])
	}
	if entry["caller_context_class"] != "none" {
		t.Fatalf("caller context=%v", entry["caller_context_class"])
	}
	for _, secret := range []string{"secret-token", "credential.example", "/private/authority", "UPDATE", strings.Repeat("a", 32), strings.Repeat("b", 64), strings.Repeat("c", 64)} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("sensitive value escaped diagnostic: %q", secret)
		}
	}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "operation": true, "phase": true, "elapsed_ms": true, "err_class": true, "caller_context_class": true, "live_before_ms": true, "transport_grant_ms": true, "live_after_ms": true, "begin_ms": true, "writer_acquisition_ms": true, "body_ms": true, "transaction_completion_ms": true, "purpose": true, "admission_queue_calls": true, "sql_begin_total_calls": true, "driver_begin_calls": true, "admission_queue_ms": true, "sql_begin_total_ms": true, "driver_begin_ms": true, "pre_driver_ms": true}
	for key := range entry {
		if !allowed[key] {
			t.Fatalf("unexpected diagnostic key %q", key)
		}
	}
	return entry
}

func TestClusterMembershipDiagnosticIdentifiesFailedPhaseWithoutSecrets(t *testing.T) {
	for _, phase := range []string{"live_before", "transport_grant", "live_after"} {
		t.Run(phase, func(t *testing.T) {
			st, id, admissions := membershipFixture(t)
			live := []cluster.Member{{ID: 1, Address: admissions[0].Address, Role: "voter"}}
			failure := errors.New("secret-token https://user:password@credential.example /private/authority")
			calls := 0
			st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
				calls++
				if phase == "live_before" || phase == "live_after" && calls == 2 {
					return nil, failure
				}
				return live, nil
			})
			key := admissions[0].KeyFingerprint
			want := failure
			if phase == "transport_grant" {
				key = strings.Repeat("f", 64)
				want = ErrClusterAdmissionRejected
			}
			output := captureClusterGrantDiagnostics(t, st)
			revision, members, err := st.GrantClusterMembership(t.Context(), id, key)
			if !errors.Is(err, want) || revision != 0 || members != nil {
				t.Fatalf("error/result changed: %d %v %v", revision, members, err)
			}
			assertClusterGrantDiagnostic(t, output, "membership", phase)
			expectedCalls := 1
			if phase == "live_after" {
				expectedCalls = 2
			}
			if calls != expectedCalls {
				t.Fatalf("live calls=%d want %d", calls, expectedCalls)
			}
			admitted, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, admissions[0].KeyFingerprint)
			wantSequence := int64(0)
			if phase == "live_after" {
				wantSequence = 1
			}
			if err != nil || admitted.MaterialGrantSequence != wantSequence {
				t.Fatalf("grant sequence=%d want%d err=%v", admitted.MaterialGrantSequence, wantSequence, err)
			}
		})
	}
}

func TestClusterWriteDiagnosticDistinguishesFailureAndUnknownCommit(t *testing.T) {
	for _, phase := range []string{"begin", "writer_acquisition", "body", "transaction_completion"} {
		t.Run(phase, func(t *testing.T) {
			st, _, _ := membershipFixture(t)
			pool, err := st.db.DB()
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("secret-token https://user:password@credential.example /private/authority")
			switch phase {
			case "begin":
				if err := pool.Close(); err != nil {
					t.Fatal(err)
				}
			case "writer_acquisition":
				if err := st.db.Callback().Raw().Before("gorm:raw").Register("test:grant-diagnostic", func(tx *gorm.DB) {
					if tx.Statement.SQL.String() == "UPDATE cluster_identities SET id = id WHERE 0" {
						tx.AddError(failure)
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { st.db.Callback().Raw().Remove("test:grant-diagnostic") })
			case "transaction_completion":
				previous := st.db.Statement.ConnPool
				st.db.Statement.ConnPool = &catalogGCUnknownCommitPool{DB: pool, failure: failure}
				t.Cleanup(func() { st.db.Statement.ConnPool = previous })
			}
			var before int64
			if phase == "body" || phase == "transaction_completion" {
				if err := pool.QueryRow("SELECT revision FROM cluster_authorizations WHERE id=1").Scan(&before); err != nil {
					t.Fatal(err)
				}
			}
			output := captureClusterGrantDiagnostics(t, st)
			bodyCalls := 0
			err = st.clusterWriteTransaction(t.Context(), func(tx *gorm.DB) error {
				bodyCalls++
				if err := tx.Exec("UPDATE cluster_authorizations SET revision=revision+1 WHERE id=1").Error; err != nil {
					return err
				}
				if phase == "body" {
					return failure
				}
				return nil
			})
			if err == nil || phase != "begin" && !errors.Is(err, failure) {
				t.Fatalf("original error changed: %v", err)
			}
			assertClusterGrantDiagnostic(t, output, "write_transaction", phase)
			wantCalls := 0
			if phase == "body" || phase == "transaction_completion" {
				wantCalls = 1
			}
			if bodyCalls != wantCalls {
				t.Fatalf("body calls=%d want%d", bodyCalls, wantCalls)
			}
			if wantCalls == 1 {
				var after int64
				if err := pool.QueryRow("SELECT revision FROM cluster_authorizations WHERE id=1").Scan(&after); err != nil {
					t.Fatal(err)
				}
				want := before
				if phase == "transaction_completion" {
					want++
				}
				if after != want {
					t.Fatalf("rollback/unknown commit changed: after%d want%d", after, want)
				}
			}
		})
	}
}

func TestClusterGrantDiagnosticThresholdAndClasses(t *testing.T) {
	st := &Store{}
	output := captureClusterGrantDiagnostics(t, st)
	st.logClusterGrantDiagnostic(t.Context(), "membership", "complete", time.Now(), nil)
	if output.Len() != 0 {
		t.Fatal("healthy fast call was logged")
	}
	st.logClusterGrantDiagnostic(t.Context(), "membership", "complete", time.Now().Add(-clusterGrantDiagnosticSlow), nil)
	entry := assertClusterGrantDiagnostic(t, output, "membership", "complete")
	if entry["err_class"] != "none" {
		t.Fatal("slow success reported as failure")
	}
	output.Reset()
	st.SetLogger(slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo})))
	st.logClusterGrantDiagnostic(t.Context(), "membership", "live_before", time.Now(), errors.New("secret-token"))
	if output.Len() != 0 {
		t.Fatal("diagnostic bypassed configured log level")
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "none"}, {context.Canceled, "canceled"}, {context.DeadlineExceeded, "deadline_exceeded"},
		{errors.Join(errors.New("secret-token"), driver.ErrBadConn), "bad_connection"},
		{ErrClusterAdmissionRejected, "admission_rejected"}, {ErrClusterIdentityConflict, "identity_conflict"},
		{ErrMaintenanceNotServing, "maintenance_not_serving"}, {ErrBusinessWritesClosed, "business_writes_closed"},
		{errors.New("database is locked: secret-token"), "other"},
		{errors.New("secret-token /private/authority"), "other"},
	} {
		if got := clusterGrantErrorClass(tc.err); got != tc.want {
			t.Fatalf("class=%s want%s", got, tc.want)
		}
	}
}

func TestClusterGrantDiagnosticSeparatesCallerCancellation(t *testing.T) {
	st, _, _ := membershipFixture(t)
	output := captureClusterGrantDiagnostics(t, st)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	err := st.clusterWriteTransaction(ctx, func(*gorm.DB) error { calls++; return nil })
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("canceled transaction changed: calls%d err%v", calls, err)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["phase"] != "begin" || entry["err_class"] != "canceled" || entry["caller_context_class"] != "canceled" {
		t.Fatalf("incorrect cancellation observation: %v", entry)
	}
	for _, key := range []string{"writer_acquisition_ms", "body_ms", "transaction_completion_ms"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("unattempted stage logged: %s", key)
		}
	}
}

func TestClusterGrantDiagnosticLoggerIsScopedPerStore(t *testing.T) {
	first, second := &Store{}, &Store{}
	firstOutput := captureClusterGrantDiagnostics(t, first)
	secondOutput := captureClusterGrantDiagnostics(t, second)
	// A scoped Store built for one transaction carries its owner's logger.
	copied := &Store{diagnosticLogger: first.diagnosticLogger}
	failure := errors.New("secret-token")
	copied.logClusterGrantDiagnostic(t.Context(), "membership", "live_before", time.Now(), failure)
	assertClusterGrantDiagnostic(t, firstOutput, "membership", "live_before")
	if secondOutput.Len() != 0 {
		t.Fatal("another Store received diagnostic")
	}
	second.logClusterGrantDiagnostic(t.Context(), "membership", "live_after", time.Now(), failure)
	assertClusterGrantDiagnostic(t, secondOutput, "membership", "live_after")
	assertClusterGrantDiagnostic(t, firstOutput, "membership", "live_before")
}
