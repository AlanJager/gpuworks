package uxstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// writeV1File writes a file as schema version 1 left it: day0's tables with
// gateway_ms holding the gateway and the preprocessing together (200 ms over
// two requests), a segment histogram cell in row 0 and an input/output cell at
// x 0 that is not a segment, plus extra tables to make the migration fail.
func writeV1File(t *testing.T, path string, extra ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	day := dayOf(ms(day0))
	// Version 1 predates the added columns as well (completePartition).
	stmts := olderShapeDDL(day)
	stmts = append(stmts,
		`ALTER TABLE `+latPrefix+day+` DROP COLUMN preprocess_ms`,
		`INSERT INTO `+latPrefix+day+` (bucket_ms, deployment_id, requests, e2e_ms, gateway_ms, queue_ms, prefill_ms,
			waiting_ms, thinking_ms, answering_ms, tail_ms, prefill_speed_tokens, prefill_speed_ms, thinking_speed_tokens,
			thinking_speed_ms, answering_speed_tokens, answering_speed_ms, decode_speed_tokens, decode_speed_ms)
			VALUES (`+itoa(floorBucket(ms(day0)))+`, 1, 2, 2000, 200, 100, 300, 0, 0, 1380, 20, 0, 0, 0, 0, 0, 0, 0, 0)`,
		`INSERT INTO `+gridPrefix+day+` VALUES (`+itoa(floorBucket(ms(day0)))+`, 1, 'seg', 0, 27, 2)`,
		`INSERT INTO `+gridPrefix+day+` VALUES (`+itoa(floorBucket(ms(day0)))+`, 1, 'io', 0, 3, 5)`,
	)
	stmts = append(stmts, extra...)
	stmts = append(stmts, `PRAGMA user_version = 1`)
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func columns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Version 2 moves what version 1 stored as the gateway's time to the
// preprocessing, where nearly all of it was spent, in the sums and in the
// histogram; the next requests split it, and opening again changes nothing.
func TestMigrateToVersion2MovesTheGatewayTimeToPreprocessing(t *testing.T) {
	c := newClock(day0)
	path := filepath.Join(t.TempDir(), "ux.db")
	writeV1File(t, path)

	s := openAt(t, path, testOpts(c))
	if v := userVersion(t, path); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	lat := latPrefix + dayOf(ms(day0))
	if cols := columns(t, s.reader, lat); !slices.Contains(cols, "gateway_ms") || !slices.Contains(cols, "preprocess_ms") {
		t.Fatalf("%s columns = %v", lat, cols)
	}
	read := func() (map[uxrecord.SegmentKind]float64, map[[2]int]int64) {
		t.Helper()
		sums, err := s.Latency(context.Background(), 1, ms(day0)-1, ms(day0)+60_000)
		if err != nil {
			t.Fatal(err)
		}
		cells := map[[2]int]int64{}
		for _, kind := range []string{uxrecord.GridSegments, uxrecord.GridIO} {
			got, err := s.Grid(context.Background(), GridQuery{DeploymentID: 1, Kind: kind, FromMS: ms(day0) - 1, ToMS: ms(day0) + 60_000})
			if err != nil {
				t.Fatal(err)
			}
			for _, cell := range got {
				if kind == uxrecord.GridIO {
					cell.X -= 1000 // keep the kinds apart in one map
				}
				cells[[2]int{cell.X, cell.Y}] += cell.N
			}
		}
		return sums.SegmentMS, cells
	}
	seg, cells := read()
	if seg[uxrecord.SegmentGateway] != 0 || seg[uxrecord.SegmentPreprocess] != 200 || seg[uxrecord.SegmentQueue] != 100 {
		t.Fatalf("migrated sums = %v, want gateway 0 and preprocessing 200", seg)
	}
	preprocessRow := uxrecord.LatencyRow("preprocess")
	if cells[[2]int{preprocessRow, 27}] != 2 || cells[[2]int{0, 27}] != 0 || cells[[2]int{-1000, 3}] != 5 {
		t.Fatalf("migrated cells = %v, want row 0's segment cell in the preprocessing row and the io cell left alone", cells)
	}

	s.Observe(engineReq("after", ms(day0)+1000))
	mustSync(t, s, false)
	seg, _ = read()
	if seg[uxrecord.SegmentGateway] != 3 || seg[uxrecord.SegmentPreprocess] != 397 {
		t.Fatalf("after a new request = %v, want gateway 3 and preprocessing 397", seg)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openAt(t, path, testOpts(c))
	if v := userVersion(t, path); v != 2 {
		t.Fatalf("user_version after reopening = %d", v)
	}
	if again, _ := read(); again[uxrecord.SegmentGateway] != 3 || again[uxrecord.SegmentPreprocess] != 397 {
		t.Fatalf("reopening changed the sums: %v", again)
	}
}

// A migration that fails partway leaves the file exactly as it was: the
// table it had already altered and the version stamp roll back together.
func TestMigrateToVersion2RollsBackAsAWhole(t *testing.T) {
	c := newClock(day0)
	path := filepath.Join(t.TempDir(), "ux.db")
	// A later day's lat_ table without gateway_ms: renaming it fails after
	// day0's table was migrated.
	writeV1File(t, path, `CREATE TABLE lat_20991231 (bucket_ms INTEGER, deployment_id INTEGER)`)

	if _, err := Open(path, testOpts(c)); err == nil {
		t.Fatal("a migration that could not finish opened the store")
	}
	if v := userVersion(t, path); v != 1 {
		t.Fatalf("user_version = %d after a failed migration, want 1", v)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lat := latPrefix + dayOf(ms(day0))
	if cols := columns(t, db, lat); !slices.Contains(cols, "gateway_ms") || slices.Contains(cols, "preprocess_ms") {
		t.Fatalf("%s columns after a failed migration = %v, want version 1's", lat, cols)
	}
	var x int
	if err := db.QueryRow(`SELECT x FROM ` + gridPrefix + dayOf(ms(day0)) + ` WHERE kind = 'seg'`).Scan(&x); err != nil || x != 0 {
		t.Fatalf("segment cell x = %d (%v), want it still in row 0", x, err)
	}
}
