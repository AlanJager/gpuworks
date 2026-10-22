package distribution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var noticeNow = time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)

func testNotice(id string) Notice {
	return Notice{
		ID: id, Severity: SeverityCritical, Kind: KindSecurity,
		Title:     map[string]string{"en": "CVE-2026-0001 in the gateway", "zh": "网关 CVE-2026-0001"},
		Body:      map[string]string{"en": "Upgrade to the latest build."},
		URL:       "https://gpuworks.ai/security/cve-2026-0001",
		ExpiresAt: noticeNow.Add(30 * 24 * time.Hour),
	}
}

func publishNotices(t *testing.T, n Notices, stores ...*objectStore) {
	t.Helper()
	payload, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Seal(payload, testKeyID, testPrivate)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stores {
		s.put(NoticesPath, envelope)
	}
}

func TestParseNoticesRefusesAnyBadNotice(t *testing.T) {
	good := testNotice("cve-2026-0001")
	for _, tc := range []struct {
		name   string
		mutate func(*Notice)
		want   string
	}{
		{"id", func(n *Notice) { n.ID = "Bad ID" }, "id must be"},
		{"severity", func(n *Notice) { n.Severity = "urgent" }, "severity"},
		{"kind", func(n *Notice) { n.Kind = "ad" }, "kind"},
		{"no english title", func(n *Notice) { n.Title = map[string]string{"zh": "只有中文"} }, "en text"},
		{"http link", func(n *Notice) { n.URL = "http://example.com" }, "https"},
		{"javascript link", func(n *Notice) { n.URL = "javascript:alert(1)" }, "https"},
		{"credentials in link", func(n *Notice) { n.URL = "https://u:p@example.com" }, "https"},
		{"forever", func(n *Notice) { n.ExpiresAt = time.Time{} }, "expires_at"},
		{"backwards", func(n *Notice) { n.StartsAt = n.ExpiresAt }, "starts_at"},
		{"long title", func(n *Notice) { n.Title["en"] = strings.Repeat("x", maxNoticeTitle+1) }, "longer"},
		{"channel", func(n *Notice) { n.When.Channels = []string{"Night ly"} }, "channel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := testNotice("cve-2026-0002")
			bad.Title = map[string]string{"en": bad.Title["en"]}
			tc.mutate(&bad)
			payload, _ := json.Marshal(Notices{Schema: NoticesSchema, Notices: []Notice{good, bad}})
			if _, err := ParseNotices(payload); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	payload, _ := json.Marshal(Notices{Schema: NoticesSchema, Notices: []Notice{good, good}})
	if _, err := ParseNotices(payload); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate ids: %v", err)
	}
	payload, _ = json.Marshal(Notices{Schema: NoticesSchema + 1})
	if _, err := ParseNotices(payload); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("future schema: %v", err)
	}
}

func TestNoticeAppliesFromLocalFactsOnly(t *testing.T) {
	facts := NoticeFacts{Channel: "nightly", BuiltAt: "2026-10-20T00:00:00Z", Vendors: []string{"nvidia"}, Now: noticeNow}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	for _, tc := range []struct {
		name string
		when NoticeWhen
		edit func(*Notice, *NoticeFacts)
		want bool
	}{
		{name: "everyone", want: true},
		{name: "channel matches", when: NoticeWhen{Channels: []string{"nightly"}}, want: true},
		{name: "other channel", when: NoticeWhen{Channels: []string{"release"}}, want: false},
		{name: "affected build", when: NoticeWhen{ServerBuiltBefore: at("2026-10-21T00:00:00Z")}, want: true},
		{name: "fixed build", when: NoticeWhen{ServerBuiltBefore: at("2026-10-19T00:00:00Z")}, want: false},
		{name: "after bound", when: NoticeWhen{ServerBuiltAfter: at("2026-10-21T00:00:00Z")}, want: false},
		{name: "vendor present", when: NoticeWhen{Vendors: []string{"ascend", "NVIDIA"}}, want: true},
		{name: "vendor absent", when: NoticeWhen{Vendors: []string{"ascend"}}, want: false},
		{name: "local build is never in a build range", when: NoticeWhen{ServerBuiltBefore: at("2030-01-01T00:00:00Z")},
			edit: func(_ *Notice, f *NoticeFacts) { f.BuiltAt = "unknown" }, want: false},
		{name: "not started", edit: func(n *Notice, _ *NoticeFacts) { n.StartsAt = noticeNow.Add(time.Hour) }, want: false},
		{name: "expired", edit: func(n *Notice, _ *NoticeFacts) { n.ExpiresAt = noticeNow }, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, f := testNotice("x"), facts
			n.When = tc.when
			if tc.edit != nil {
				tc.edit(&n, &f)
			}
			if got := n.Applies(f); got != tc.want {
				t.Fatalf("applies = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFetchNoticesTakesTheFirstThatVerifies(t *testing.T) {
	slow, good := newObjectStore(t), newObjectStore(t)
	n := Notices{Schema: NoticesSchema, PublishedAt: noticeNow, Notices: []Notice{testNotice("cve-2026-0001")}}
	publishNotices(t, n, slow, good)
	slow.fail(NoticesPath, fault{delay: 2 * time.Second})
	start := time.Now()
	got, err := clientFor(slow, good).FetchNotices(context.Background())
	if err != nil || len(got.Notices.Notices) != 1 || got.Source != good.URL() || got.KeyID != testKeyID {
		t.Fatalf("fetch = %+v, %v", got, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("waited for the slow source")
	}
}

func TestFetchNoticesUnpublishedIsEmpty(t *testing.T) {
	a, b := newObjectStore(t), newObjectStore(t)
	b.fail(NoticesPath, fault{status: 503})
	got, err := clientFor(a, b).FetchNotices(context.Background())
	if err != nil || got == nil || len(got.Notices.Notices) != 0 {
		t.Fatalf("fetch = %+v, %v; want no notices and no error", got, err)
	}
}

func TestFetchNoticesRefusesForgeries(t *testing.T) {
	forged, missing := newObjectStore(t), newObjectStore(t)
	payload, _ := json.Marshal(Notices{Schema: NoticesSchema, Notices: []Notice{testNotice("phish")}})
	envelope, _ := json.Marshal(Envelope{Payload: payload, Signatures: []Signature{{KeyID: testKeyID, Sig: make([]byte, 64)}}})
	forged.put(NoticesPath, envelope)
	_, err := clientFor(forged, missing).FetchNotices(context.Background())
	if err == nil || !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("err = %v, want a refused file, not an empty one", err)
	}

	unsigned := newObjectStore(t)
	unsigned.put(NoticesPath, payload) // a plain JSON file, no envelope
	if _, err := clientFor(unsigned).FetchNotices(context.Background()); err == nil {
		t.Fatal("an unsigned file was accepted")
	}

	down := newObjectStore(t)
	down.fail(NoticesPath, fault{status: 500})
	if _, err := clientFor(down).FetchNotices(context.Background()); err == nil {
		t.Fatal("an unreachable channel was taken for one without notices")
	}
}
