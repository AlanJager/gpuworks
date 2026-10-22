package modelcache

import (
	"reflect"
	"testing"
	"time"
)

const gib = int64(1) << 30

func at(days int) *time.Time {
	t := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -days)
	return &t
}

func TestPlanNeedsNoEvictionWhenItFits(t *testing.T) {
	got := Plan(Input{NeedBytes: 10 * gib, FreeBytes: 100 * gib, ReserveBytes: 50 * gib, Entries: []Entry{{ID: 1, SizeBytes: 5 * gib, Backed: true}}})
	if got.Refusal != "" || len(got.Evict) != 0 || got.FreedBytes != 0 {
		t.Fatalf("plan = %+v, want no eviction", got)
	}
}

func TestPlanEvictsLeastRecentlyUsedUntilTheReserveHolds(t *testing.T) {
	entries := []Entry{
		{ID: 1, SizeBytes: 1 * gib, LastUsed: at(41), Backed: true},
		{ID: 2, SizeBytes: 1 * gib, LastUsed: at(20), Backed: true},
		{ID: 3, SizeBytes: 57 * gib, LastUsed: at(15), Backed: true},
		{ID: 4, SizeBytes: 28 * gib, LastUsed: at(12), Backed: false},                   // no backing copy
		{ID: 5, SizeBytes: 27 * gib, LastUsed: at(6), Backed: true, Transferring: true}, // still copying
		{ID: 6, SizeBytes: 15 * gib, LastUsed: at(0), Backed: true, Referenced: true},   // in use
	}
	got := Plan(Input{NeedBytes: 105 * gib, FreeBytes: 126 * gib, ReserveBytes: 50 * gib, Entries: entries})
	if got.Refusal != "" {
		t.Fatalf("refused: %+v", got)
	}
	if want := []uint{1, 2, 3}; !reflect.DeepEqual(got.Evict, want) {
		t.Fatalf("evict = %v, want %v", got.Evict, want)
	}
	if got.FreedBytes != 59*gib || got.NeedDiskBytes != 29*gib {
		t.Fatalf("freed=%d need=%d", got.FreedBytes/gib, got.NeedDiskBytes/gib)
	}
}

func TestPlanStopsAsSoonAsEnoughIsFree(t *testing.T) {
	entries := []Entry{
		{ID: 7, SizeBytes: 30 * gib, LastUsed: at(3), Backed: true},
		{ID: 8, SizeBytes: 30 * gib, LastUsed: at(9), Backed: true},
	}
	got := Plan(Input{NeedBytes: 20 * gib, FreeBytes: 60 * gib, ReserveBytes: 50 * gib, Entries: entries})
	if want := []uint{8}; !reflect.DeepEqual(got.Evict, want) {
		t.Fatalf("evict = %v, want the older one only", got.Evict)
	}
}

func TestPlanUnusedEntriesAreOldest(t *testing.T) {
	entries := []Entry{
		{ID: 2, SizeBytes: 10 * gib, LastUsed: at(100), Backed: true},
		{ID: 9, SizeBytes: 10 * gib, Backed: true},
	}
	got := Plan(Input{NeedBytes: 10 * gib, FreeBytes: 0, ReserveBytes: 0, Entries: entries})
	if want := []uint{9}; !reflect.DeepEqual(got.Evict, want) {
		t.Fatalf("evict = %v, want the never-used entry first", got.Evict)
	}
}

func TestPlanHonoursTheCacheCap(t *testing.T) {
	entries := []Entry{
		{ID: 1, SizeBytes: 60 * gib, LastUsed: at(5), Backed: true},
		{ID: 2, SizeBytes: 60 * gib, LastUsed: at(1), Backed: true, Referenced: true},
	}
	got := Plan(Input{NeedBytes: 30 * gib, FreeBytes: 1000 * gib, ReserveBytes: 50 * gib, CapBytes: 140 * gib, Entries: entries})
	if want := []uint{1}; got.Refusal != "" || !reflect.DeepEqual(got.Evict, want) {
		t.Fatalf("plan = %+v, want entry 1 evicted for the cap", got)
	}
	if got.NeedCapBytes != 10*gib {
		t.Fatalf("need cap = %d", got.NeedCapBytes/gib)
	}
}

func TestPlanRefusesAModelLargerThanTheCap(t *testing.T) {
	got := Plan(Input{NeedBytes: 438 * gib, FreeBytes: 1000 * gib, CapBytes: 200 * gib})
	if got.Refusal != RefuseCacheLimit || len(got.Evict) != 0 {
		t.Fatalf("plan = %+v, want cache limit refusal", got)
	}
}

func TestPlanRefusesWhenEvictingEverythingIsNotEnough(t *testing.T) {
	entries := []Entry{
		{ID: 1, SizeBytes: 10 * gib, Backed: true},
		{ID: 2, SizeBytes: 90 * gib, Backed: true, Referenced: true},
	}
	got := Plan(Input{NeedBytes: 100 * gib, FreeBytes: 60 * gib, ReserveBytes: 50 * gib, Entries: entries})
	if got.Refusal != RefuseInsufficientDisk || len(got.Evict) != 0 {
		t.Fatalf("plan = %+v, want insufficient disk without a partial eviction", got)
	}
}

// The live case on a 2 GiB cap: a 1.5 GB model a running deployment reads
// leaves no room for a 1 GB one, although the disk has plenty.
func TestPlanRefusesWhenTheCapIsTakenByModelsInUse(t *testing.T) {
	entries := []Entry{{ID: 7, SizeBytes: 1519182438, Backed: true, Referenced: true}}
	got := Plan(Input{NeedBytes: 999602609, FreeBytes: 400 * gib, ReserveBytes: 20 * gib, CapBytes: 2 * gib, Entries: entries})
	if got.Refusal != RefuseCacheInUse || len(got.Evict) != 0 || got.NeedDiskBytes != 0 || got.NeedCapBytes <= 0 {
		t.Fatalf("plan = %+v, want the cache-in-use refusal", got)
	}
	// Short of disk as well: the disk is the reason to report.
	got = Plan(Input{NeedBytes: 999602609, FreeBytes: 20 * gib, ReserveBytes: 20 * gib, CapBytes: 2 * gib, Entries: entries})
	if got.Refusal != RefuseInsufficientDisk {
		t.Fatalf("plan = %+v, want insufficient disk", got)
	}
}

func TestPlanBreaksLastUsedTiesByID(t *testing.T) {
	same := at(2)
	entries := []Entry{{ID: 5, SizeBytes: gib, LastUsed: same, Backed: true}, {ID: 3, SizeBytes: gib, LastUsed: same, Backed: true}}
	got := Plan(Input{NeedBytes: gib, FreeBytes: 0, Entries: entries})
	if want := []uint{3}; !reflect.DeepEqual(got.Evict, want) {
		t.Fatalf("evict = %v, want %v", got.Evict, want)
	}
}
