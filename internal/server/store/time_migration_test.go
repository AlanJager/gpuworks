package store

import (
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestNormalizeStoredTimesToUTC(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:normalize_times?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE audit_rows (
		id INTEGER PRIMARY KEY,
		created_at DATETIME,
		updated_at DATETIME,
		last_used DATETIME,
		label TEXT
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO audit_rows(id, created_at, updated_at, last_used, label) VALUES
		(1, '2026-08-19 06:35:50.65842+08:00', '2026-08-19 06:35:50.658421+08:00', '2026-08-19T00:05:00-03:30', 'unchanged')`).Error; err != nil {
		t.Fatal(err)
	}

	if err := normalizeStoredTimesToUTC(db); err != nil {
		t.Fatal(err)
	}

	var row struct {
		CreatedAt string
		UpdatedAt string
		LastUsed  string
		Label     string
	}
	if err := db.Raw(`SELECT CAST(created_at AS TEXT) created_at, CAST(updated_at AS TEXT) updated_at,
		CAST(last_used AS TEXT) last_used, label FROM audit_rows WHERE id = 1`).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"created_at": row.CreatedAt,
		"updated_at": row.UpdatedAt,
		"last_used":  row.LastUsed,
	} {
		if !strings.HasSuffix(value, "+00:00") {
			t.Fatalf("%s = %q, want UTC offset", name, value)
		}
	}
	if !strings.Contains(row.CreatedAt, ".65842") {
		t.Fatalf("created_at = %q, five-digit fractional seconds were not preserved", row.CreatedAt)
	}
	if !strings.Contains(row.UpdatedAt, ".658421") {
		t.Fatalf("updated_at = %q, six-digit fractional seconds were not preserved", row.UpdatedAt)
	}
	if row.Label != "unchanged" {
		t.Fatalf("label = %q", row.Label)
	}

	created, err := parseStoredTime(row.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 18, 22, 35, 50, 658420000, time.UTC)
	if !created.Equal(want) {
		t.Fatalf("created_at = %s, want %s", created, want)
	}
}

func TestParseStoredTimeTreatsOffsetlessLegacyValueAsUTC(t *testing.T) {
	got, err := parseStoredTime("2026-08-19 06:35:50.123456")
	if err != nil {
		t.Fatal(err)
	}
	if got.Location() != time.UTC || got.Hour() != 6 {
		t.Fatalf("got %s (%s), want 06:xx UTC", got, got.Location())
	}
}
