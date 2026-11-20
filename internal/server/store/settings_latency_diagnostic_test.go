package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func settingDiagnosticRecords(t *testing.T, output string) map[string]map[string]any {
	t.Helper()
	rows := make(map[string]map[string]any)
	for _, line := range bytes.Split(bytes.TrimSpace([]byte(output)), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row["msg"] != "setting write diagnostic" {
			continue
		}
		phase, _ := row["phase"].(string)
		if rows[phase] != nil {
			t.Fatalf("duplicate phase%s: %s", phase, output)
		}
		rows[phase] = row
		if row["operation"] != "setting_write" || row["level"] != "DEBUG" {
			t.Fatalf("wrong operation/level: %v", row)
		}
		if ms, ok := row["elapsed_ms"].(float64); !ok || ms < 0 {
			t.Fatalf("invalid elapsed: %v", row)
		}
	}
	if strings.Contains(output, "DO-NOT-LOG") || strings.Contains(output, "/private/authority") {
		t.Fatalf("sensitive diagnostic: %s", output)
	}
	return rows
}

func TestSettingLatencyDiagnosticsPreserveTransactionAndNotificationResults(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "settings.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	output := captureClusterGrantDiagnostics(t, st)
	key := SettingKey("DO-NOT-LOG-setting-key")
	const value = `"DO-NOT-LOG-setting-value"`
	t.Run("confirmed-target-and-local-hint-phases-without-notification-sql", func(t *testing.T) {
		output.Reset()
		var queries int
		if err := st.db.Callback().Query().Before("gorm:query").Register("test:notification-barrier", func(tx *gorm.DB) {
			_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
			if tx.Statement.Table == "committed_notifications" && !transaction {
				queries++
				tx.AddError(errors.New("DO-NOT-LOG-unexpected-postcommit-sql"))
			}
		}); err != nil {
			t.Fatal(err)
		}
		defer st.db.Callback().Query().Remove("test:notification-barrier")
		if err := st.SetBusinessSetting(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
		if queries != 0 || st.GetSetting(key) != value {
			t.Fatalf("business hint ran notification SQL or lost target: queries=%d", queries)
		}
		rows := settingDiagnosticRecords(t, output.String())
		for _, phase := range []string{"transaction_returned", "post_commit_notify_call_returned"} {
			row := rows[phase]
			if row == nil || row["err_class"] != "none" || row["caller_context_class"] != "none" {
				t.Fatalf("missing successful phase %s: %s", phase, output.String())
			}
		}
	})
	t.Run("canceled-target", func(t *testing.T) {
		output.Reset()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := st.SetBusinessSetting(ctx, key, `"not committed"`); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation changed: %v", err)
		}
		if st.GetSetting(key) != value {
			t.Fatal("canceled target changed value")
		}
		rows := settingDiagnosticRecords(t, output.String())
		row := rows["transaction_returned"]
		if len(rows) != 1 || row["err_class"] != "canceled" || row["caller_context_class"] != "canceled" {
			t.Fatalf("canceled phase/class changed: %s", output.String())
		}
	})
	t.Run("notification-sql-failure-affects-only-internal-publisher", func(t *testing.T) {
		output.Reset()
		injected := errors.New("DO-NOT-LOG-notification-error /private/authority")
		const changed = `"DO-NOT-LOG-postcommit-changed-value"`
		var queries int
		if err := st.db.Callback().Query().Before("gorm:query").Register("test:notification-failure", func(tx *gorm.DB) {
			_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
			if tx.Statement.Table == "committed_notifications" && !transaction {
				queries++
				tx.AddError(injected)
			}
		}); err != nil {
			t.Fatal(err)
		}
		defer st.db.Callback().Query().Remove("test:notification-failure")
		if err := st.SetBusinessSetting(t.Context(), key, changed); err != nil {
			t.Fatalf("notify error changed committed result: %v", err)
		}
		if st.GetSetting(key) != changed {
			t.Fatal("committed target missing")
		}
		rows := settingDiagnosticRecords(t, output.String())
		if rows["transaction_returned"]["err_class"] != "none" || rows["post_commit_notify_call_returned"]["err_class"] != "none" || queries != 0 {
			t.Fatalf("phase errors missing: %s", output.String())
		}
		// Internal defaults write the row and hint the local bus only: they
		// never read the notification table, and they log no business phases.
		output.Reset()
		if err := st.SetSetting(key, value); err != nil || queries != 0 || st.GetSetting(key) != value {
			t.Fatalf("internal default write changed: queries=%d err=%v", queries, err)
		}
		if err := st.publishCommittedNotification(settingNotifyKey(string(key))); !errors.Is(err, injected) {
			t.Fatalf("generic publisher lost original notification error: %v", err)
		}
		if len(settingDiagnosticRecords(t, output.String())) != 0 {
			t.Fatalf("internal notification failure leaked business diagnostics: %s", output.String())
		}
	})
	t.Run("target-error-phase-is-private-and-does-not-claim-notification", func(t *testing.T) {
		output.Reset()
		cause := errors.New("DO-NOT-LOG-target-error /private/authority")
		const callback = "test:setting-diagnostic-target-failure"
		if err := st.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "settings" {
				tx.AddError(cause)
			}
		}); err != nil {
			t.Fatal(err)
		}
		defer st.db.Callback().Create().Remove(callback)
		if err := st.SetBusinessSetting(t.Context(), "DO-NOT-LOG-failed-target", value); !errors.Is(err, cause) {
			t.Fatalf("target error changed: %v", err)
		}
		rows := settingDiagnosticRecords(t, output.String())
		row := rows["transaction_returned"]
		if len(rows) != 1 || row["err_class"] != "other" || row["caller_context_class"] != "none" {
			t.Fatalf("failed target phase/class changed: %s", output.String())
		}
	})
	t.Run("nil-context-diagnostics-only-normalization", func(t *testing.T) {
		output.Reset()
		var absent context.Context // a caller that passes no context at all
		if err := st.SetBusinessSetting(absent, key, value); err != nil {
			t.Fatal(err)
		}
		rows := settingDiagnosticRecords(t, output.String())
		if len(rows) != 2 || rows["transaction_returned"]["caller_context_class"] != "none" {
			t.Fatalf("nil caller context: %s", output.String())
		}
	})
	t.Run("internal-default-not-logged", func(t *testing.T) {
		output.Reset()
		if err := st.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
		if len(settingDiagnosticRecords(t, output.String())) != 0 {
			t.Fatalf("internal startup write logged: %s", output.String())
		}
	})
}
