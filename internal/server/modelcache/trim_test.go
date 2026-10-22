package modelcache

import "testing"

func TestTrimKeepsSafePartialEvictionWithoutChangingAdmission(t *testing.T) {
	in := Input{FreeBytes: 1000, CapBytes: 200, Entries: []Entry{
		{ID: 1, SizeBytes: 60, Backed: true},
		{ID: 2, SizeBytes: 240, Backed: true, Referenced: true},
		{ID: 3, SizeBytes: 20, Backed: true, Transferring: true},
		{ID: 4, SizeBytes: 30},
	}}
	trim := Trim(in)
	if trim.Refusal != RefuseCacheInUse || len(trim.Evict) != 1 || trim.Evict[0] != 1 || trim.FreedBytes != 60 {
		t.Fatalf("trim=%+v", trim)
	}
	if got := Plan(in); got.Refusal != RefuseCacheInUse || len(got.Evict) != 0 {
		t.Fatalf("admission=%+v", got)
	}
	in.CapBytes = 0
	if got := Trim(in); len(got.Evict) != 0 || got.Refusal != "" {
		t.Fatalf("unlimited=%+v", got)
	}
	in.FreeBytes = 0
	in.ReserveBytes = 40
	if got := Trim(in); len(got.Evict) != 1 || got.FreedBytes != 60 || got.Refusal != "" {
		t.Fatalf("disk reserve=%+v", got)
	}
}
