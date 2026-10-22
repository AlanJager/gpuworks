package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestNoticeReadsMarkListAndPrune(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
	if err := s.MarkNoticesRead(1, []string{"cve-1", "event-1"}, at); err != nil {
		t.Fatal(err)
	}
	// Marking again keeps the first time and adds nothing.
	if err := s.MarkNoticesRead(1, []string{"cve-1"}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNoticesRead(2, []string{"cve-1"}, at); err != nil {
		t.Fatal(err)
	}
	var first NoticeRead
	if err := s.db.First(&first, "user_id = 1 AND notice_id = 'cve-1'").Error; err != nil || !first.ReadAt.Equal(at) {
		t.Fatalf("first mark = %+v, %v", first, err)
	}
	reads, err := s.NoticeReads(1)
	if err != nil || len(reads) != 2 || !reads["cve-1"] || !reads["event-1"] {
		t.Fatalf("reads = %v, %v", reads, err)
	}
	if reads, _ := s.NoticeReads(3); len(reads) != 0 {
		t.Fatalf("another user's reads = %v", reads)
	}
	if err := s.DeleteNoticeReadsExcept([]string{"cve-1"}); err != nil {
		t.Fatal(err)
	}
	if reads, _ := s.NoticeReads(1); len(reads) != 1 || !reads["cve-1"] {
		t.Fatalf("after prune = %v", reads)
	}
	if err := s.DeleteNoticeReadsExcept(nil); err != nil {
		t.Fatal(err)
	}
	var n int64
	if s.db.Model(&NoticeRead{}).Count(&n); n != 0 {
		t.Fatalf("%d marks left when no notice is served", n)
	}
}

func TestUsageReportLastDayRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if day, err := s.UsageReportLastDay(); err != nil || day != "" {
		t.Fatalf("fresh = %q, %v", day, err)
	}
	if err := s.SetUsageReportLastDay("2026-10-21"); err != nil {
		t.Fatal(err)
	}
	if day, err := s.UsageReportLastDay(); err != nil || day != "2026-10-21" {
		t.Fatalf("day = %q, %v", day, err)
	}
	// Stored as JSON, like every setting, so the settings list stays valid.
	if raw := s.GetSetting(SettingUsageReportLastDay); raw != `"2026-10-21"` {
		t.Fatalf("stored %q", raw)
	}
}

func noticeReadsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			_ = sql.Close()
		}
	})
	return db
}

func TestNoticeReadsMigrationCreatesItsTableOnce(t *testing.T) {
	db := noticeReadsDB(t)
	m := migrationByName(t, "notice_reads")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("notice_reads") {
		t.Fatal("notice_reads is missing")
	}
	for _, col := range []string{"user_id", "notice_id", "read_at"} {
		if !db.Migrator().HasColumn("notice_reads", col) {
			t.Fatalf("column %s is missing", col)
		}
	}
	if err := db.Exec(`INSERT INTO notice_reads (user_id, notice_id, read_at) VALUES (1, 'a', '2026-10-22 00:00:00')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO notice_reads (user_id, notice_id, read_at) VALUES (1, 'a', '2026-10-22 00:00:00')`).Error; err == nil {
		t.Fatal("the same user and notice were stored twice")
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", m.Version, m.Name).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d, error=%v", markers, err)
	}
}

func TestNoticeReadsMigrationRollsBackAndCanRetry(t *testing.T) {
	db := noticeReadsDB(t)
	m := migrationByName(t, "notice_reads")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_notice_reads_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasTable("notice_reads") {
		t.Fatal("the table survived a failed migration")
	}
	if err := db.Exec(`DROP TRIGGER fail_notice_reads_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil || !db.Migrator().HasTable("notice_reads") {
		t.Fatalf("retry after the injected failure: %v", err)
	}
}
