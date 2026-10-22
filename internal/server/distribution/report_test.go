package distribution

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

// sampleReport is a fleet report as usagereport.FleetReport encodes it.
func sampleReport() url.Values {
	return url.Values{"id": {"0123456789abcdef"}, "v": {"v0.9.1"}, "nodes": {"5"}, "online": {"4"},
		"agent": {"v0.9.1:4", "v0.9.0:1"}, "gpu": {"NVIDIA A100-SXM4-80GB:16", "NVIDIA H20:8"}}
}

func useDefaultSources(t *testing.T, stores ...*objectStore) *Client {
	t.Helper()
	saved := DefaultSources
	DefaultSources = nil
	for _, s := range stores {
		DefaultSources = append(DefaultSources, s.URL())
	}
	t.Cleanup(func() { DefaultSources = saved })
	return &Client{Keys: testKeys(), IndexTimeout: 2 * time.Second, StallTimeout: 300 * time.Millisecond}
}

// The built-in sources receive the report on the index request.
func TestFetchIndexReportingSendsTheReportToDefaultSources(t *testing.T) {
	a, b := newObjectStore(t), newObjectStore(t)
	publish(t, testIndex("nightly"), a, b)
	client := useDefaultSources(t, a, b)
	if _, err := client.ProbeIndex(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*objectStore{a, b} {
		if q := s.queryLog(); len(q) != 1 || q[0] != "" {
			t.Fatalf("a plain index fetch carried %q", q)
		}
	}
	report := sampleReport()
	var fetched *Fetched
	fetched, err := client.FetchIndexReporting(context.Background(), "nightly", report)
	if err != nil || fetched.Index.Server.ReleaseID != "abc1234" {
		t.Fatalf("fetch = %+v, %v", fetched, err)
	}
	// Both sources were asked at once; whichever answered, the parameters are the report's.
	deadline := time.Now().Add(2 * time.Second)
	for len(a.queryLog())+len(b.queryLog()) < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for _, s := range []*objectStore{a, b} {
		log := s.queryLog()
		if len(log) < 2 || log[1] != report.Encode() {
			t.Fatalf("source saw %q, want %q", log, report.Encode())
		}
	}
}

// Sources the operator chose (--source, an internal release site) are asked
// for the index and told nothing.
func TestFetchIndexReportingSaysNothingToCustomSources(t *testing.T) {
	store := newObjectStore(t)
	publish(t, testIndex("nightly"), store)
	client := clientFor(store)
	if _, err := client.FetchIndexReporting(context.Background(), "nightly", sampleReport()); err != nil {
		t.Fatal(err)
	}
	if q := store.queryLog(); len(q) != 1 || q[0] != "" {
		t.Fatalf("custom source saw %q", q)
	}
}

// The report is built only when the sources are actually asked, and a cached
// check is not asked again.
func TestCheckCacheBuildsTheReportOnlyForARealCheck(t *testing.T) {
	store := newObjectStore(t)
	publish(t, testIndex("nightly"), store)
	client := useDefaultSources(t, store)
	var cache CheckCache
	built := 0
	report := func() url.Values { built++; return sampleReport() }
	for i := 0; i < 3; i++ {
		if _, err, _ := cache.Probe(context.Background(), client, "nightly", false, report); err != nil {
			t.Fatal(err)
		}
	}
	if built != 1 {
		t.Fatalf("report built %d times for one real check", built)
	}
	if q := store.queryLog(); len(q) != 1 || !strings.Contains(q[0], "nodes=5") {
		t.Fatalf("the one request carried %q", q)
	}
	if _, err, _ := cache.Probe(context.Background(), client, "nightly", true, nil); err != nil {
		t.Fatal(err)
	}
	if q := store.queryLog(); len(q) != 2 || q[1] != "" {
		t.Fatalf("a check without a report carried %q", q)
	}
}

func TestCheckCacheCachedAnswersWithoutAsking(t *testing.T) {
	store := newObjectStore(t)
	publish(t, testIndex("nightly"), store)
	client := clientFor(store)
	var cache CheckCache
	if _, _, _, ok := cache.Cached("nightly"); ok {
		t.Fatal("an empty cache answered")
	}
	cache.Probe(context.Background(), client, "nightly", false, nil)
	if fetched, err, at, ok := cache.Cached("nightly"); !ok || err != nil || fetched == nil || at.IsZero() {
		t.Fatalf("cached = %+v, %v, %v, %v", fetched, err, at, ok)
	}
	if _, _, _, ok := cache.Cached("release"); ok {
		t.Fatal("another channel's check answered")
	}
	if got := indexRequests(store, "nightly"); got != 1 {
		t.Fatalf("Cached asked the source: %d requests", got)
	}
}
