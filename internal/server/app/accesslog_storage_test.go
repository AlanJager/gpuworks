package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/glebarez/go-sqlite"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TestAFullDiskAnswerNamesTheServersDataDirectory goes through the server's
// own access log: the handler only has the error, and the data directory
// reaches the answer from the writer the log wraps every request in.
func TestAFullDiskAnswerNamesTheServersDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	full := sqliteFullError(t, filepath.Join(dataDir, "full.db"))

	var logs strings.Builder
	h := accessLogHandlerFor(slog.New(slog.NewTextHandler(&logs, nil)), dataDir, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		api.WriteError(w, http.StatusInternalServerError, full)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models", nil))

	var body struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInsufficientStorage || body.Code != "SERVER_STORAGE_FULL" || body.Params["path"] != dataDir {
		t.Fatalf("answer = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := body.Params["available_bytes"]; !ok {
		t.Fatalf("answer = %s, want the filesystem's room", rec.Body.String())
	}
	if out := logs.String(); !strings.Contains(out, "code=SERVER_STORAGE_FULL") || !strings.Contains(out, "status=507") {
		t.Fatalf("access log = %q, want the code and status on the request's line", out)
	}
}

// sqliteFullError is SQLite's answer to a write with no room: a page cap on a
// real database file stands in for the disk.
func sqliteFullError(t *testing.T, path string) error {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var pages int
	if _, err := db.Exec("CREATE TABLE t (x BLOB)"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO t VALUES (randomblob(1048576))")
	if !store.IsStorageFull(err) {
		t.Fatalf("write past the cap = %v, want SQLITE_FULL", err)
	}
	return err
}
