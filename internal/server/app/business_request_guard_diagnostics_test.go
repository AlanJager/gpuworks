//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestBusinessStoreGuardLogsCanceledGrantWithoutRequestIdentity(t *testing.T) {
	var output bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	handler := guardBusinessRequestsForStore(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("business handler entered after canceled grant")
	}), new(store.Store), store.ClusterIdentity{ClusterID: "private-cluster"}, 2, "private-member-key", log)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/settings/private-path?token=private-token", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer private-jwt")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "member business service is not ready\n" {
		t.Fatalf("grant refusal changed: status=%d body=%q", response.Code, response.Body.String())
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("missing grant diagnostic: %v; output=%q", err, output.String())
	}
	if record["level"] != "DEBUG" || record["msg"] != "business membership grant failed" || record["err_class"] != "canceled" {
		t.Fatalf("grant diagnostic = %v", record)
	}
	if duration, ok := record["grant_ms"].(float64); !ok || duration < 0 {
		t.Fatalf("missing grant duration: %v", record)
	}
	if strings.Contains(output.String(), "private-") {
		t.Fatalf("diagnostic exposed request identity: %s", output.String())
	}
}

func TestClosedMemberErrorClassOmitsWrappedErrorDetails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		class string
	}{
		{"canceled", context.Canceled, "canceled"},
		{"deadline", context.DeadlineExceeded, "deadline_exceeded"},
		{"connection", driver.ErrBadConn, "bad_connection"},
		{"admission", store.ErrClusterAdmissionRejected, "admission_rejected"},
		{"identity", store.ErrClusterIdentityConflict, "identity_conflict"},
		{"maintenance", store.ErrMaintenanceNotServing, "maintenance_not_serving"},
		{"business_closed", store.ErrBusinessWritesClosed, "business_writes_closed"},
		// The class comes from the error's identity, never from its text.
		{"joined_lock", errors.Join(store.ErrBusinessWritesClosed, errors.New("database is locked")), "business_writes_closed"},
		{"lock_text_only", errors.New("database is locked"), "other"},
		{"unknown", errors.New("private-path private-key private-jwt"), "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("private-path private-key private-jwt: %w", tc.cause)
			if got := closedMemberErrorClass(err); got != tc.class {
				t.Fatalf("safe error classification = %q, want %q", got, tc.class)
			}
		})
	}
}
