package app

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

type noticeSource struct {
	mu       sync.Mutex
	body     []byte
	status   int
	requests int
}

func (s *noticeSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	if r.URL.Path != "/"+distribution.NoticesPath {
		http.NotFound(w, r)
		return
	}
	if s.status != 0 {
		w.WriteHeader(s.status)
		return
	}
	if s.body == nil {
		http.NotFound(w, r)
		return
	}
	w.Write(s.body)
}

func (s *noticeSource) publish(t *testing.T, key ed25519.PrivateKey, notices ...distribution.Notice) {
	t.Helper()
	payload, err := json.Marshal(distribution.Notices{Schema: distribution.NoticesSchema, Notices: notices})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := distribution.Seal(payload, "test", key)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.body = envelope
	s.mu.Unlock()
}

type noticeRig struct {
	rig     *usageRig
	notices *noticeService
	source  *noticeSource
	key     ed25519.PrivateKey
	logs    *bytes.Buffer
}

func newNoticeRig(t *testing.T) *noticeRig {
	t.Helper()
	rig := newUsageRig(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	source := &noticeSource{}
	srv := httptest.NewServer(source)
	t.Cleanup(srv.Close)
	client := &distribution.Client{Sources: []string{srv.URL}, Keys: distribution.Keys{"test": public}, IndexTimeout: 2 * time.Second}
	logs := &bytes.Buffer{}
	n := &noticeService{store: rig.store, client: func() *distribution.Client { return client }, settings: rig.settings,
		log: slog.New(slog.NewTextHandler(logs, nil)), now: func() time.Time { return rig.now },
		started: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
	return &noticeRig{rig: rig, notices: n, source: source, key: private, logs: logs}
}

func notice(id, severity, kind string, published time.Time) distribution.Notice {
	return distribution.Notice{ID: id, Severity: severity, Kind: kind, Title: map[string]string{"en": id, "zh": id + " 中文"},
		URL: "https://gpuworks.ai/n/" + id, StartsAt: published, ExpiresAt: published.Add(90 * 24 * time.Hour)}
}

func TestNoticesListWhatAppliesMostUrgentFirst(t *testing.T) {
	r := newNoticeRig(t)
	r.rig.now = time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	base := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	ascendOnly := notice("cve-ascend", distribution.SeverityCritical, distribution.KindSecurity, base)
	ascendOnly.When.Vendors = []string{"ascend"}
	r.source.publish(t, r.key,
		notice("event-1", distribution.SeverityInfo, distribution.KindEvent, base.Add(2*time.Hour)),
		notice("cve-1", distribution.SeverityCritical, distribution.KindSecurity, base),
		notice("release-1", distribution.SeverityImportant, distribution.KindRelease, base.Add(time.Hour)),
		ascendOnly,
	)
	n, err := r.rig.store.RegisterNode("n1", store.HashToken("t"), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.rig.store.DB().Model(&store.Node{}).Where("id = ?", n.ID).Update("gpu_vendor", "nvidia").Error; err != nil {
		t.Fatal(err)
	}
	r.notices.fetch(t.Context())

	list, err := r.notices.List(1)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range list.Notices {
		ids = append(ids, x.ID)
	}
	if strings.Join(ids, ",") != "cve-1,release-1,event-1" || list.Unread != 3 {
		t.Fatalf("notices = %v, unread %d", ids, list.Unread)
	}
	if list.Notices[0].Title["zh"] != "cve-1 中文" || list.Notices[0].URL != "https://gpuworks.ai/n/cve-1" || !list.Notices[0].PublishedAt.Equal(base) {
		t.Fatalf("first = %+v", list.Notices[0])
	}

	if err := r.notices.MarkRead(1, []string{"cve-1"}); err != nil {
		t.Fatal(err)
	}
	if list, _ := r.notices.List(1); list.Unread != 2 || !list.Notices[0].Read {
		t.Fatalf("after read = %+v", list)
	}
	if list, _ := r.notices.List(2); list.Unread != 3 {
		t.Fatalf("another user's bell = %+v", list)
	}

}

func TestNoticesFetchPrunesReadMarksAndLogsSecurityOnce(t *testing.T) {
	r := newNoticeRig(t)
	r.rig.now = time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	base := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	cve := notice("cve-1", distribution.SeverityCritical, distribution.KindSecurity, base)
	r.source.publish(t, r.key, cve, notice("event-1", distribution.SeverityInfo, distribution.KindEvent, base))
	r.notices.fetch(t.Context())
	r.notices.fetch(t.Context())
	if got := strings.Count(r.logs.String(), "security notice"); got != 1 {
		t.Fatalf("security notice logged %d times:\n%s", got, r.logs.String())
	}
	if err := r.notices.MarkRead(1, []string{"cve-1", "event-1"}); err != nil {
		t.Fatal(err)
	}
	// The publisher withdraws the event: its mark goes, the others stay.
	r.source.publish(t, r.key, cve)
	r.notices.fetch(t.Context())
	reads, _ := r.rig.store.NoticeReads(1)
	if len(reads) != 1 || !reads["cve-1"] {
		t.Fatalf("reads after the withdrawal = %v", reads)
	}
	// A channel that cannot be read leaves the notices and the marks alone.
	r.source.mu.Lock()
	r.source.status = http.StatusBadGateway
	r.source.mu.Unlock()
	r.notices.fetch(t.Context())
	if list, _ := r.notices.List(1); len(list.Notices) != 1 {
		t.Fatalf("notices after a failed fetch = %+v", list.Notices)
	}
	if reads, _ := r.rig.store.NoticeReads(1); len(reads) != 1 {
		t.Fatalf("reads after a failed fetch = %v", reads)
	}
}

// A notice for builds before a fix applies to this server only when it is
// one of them; a local build (no build time) is never in a build range.
func TestNoticesMatchTheServerBuild(t *testing.T) {
	r := newNoticeRig(t)
	r.rig.now = time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	saved := version.BuildDate
	t.Cleanup(func() { version.BuildDate = saved })
	fixed := notice("cve-fixed", distribution.SeverityCritical, distribution.KindSecurity, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	fixed.When.ServerBuiltBefore = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	r.source.publish(t, r.key, fixed)
	r.notices.fetch(t.Context())
	for build, want := range map[string]int{"2026-09-01T00:00:00Z": 1, "2026-10-15T00:00:00Z": 0, "unknown": 0} {
		version.BuildDate = build
		list, _ := r.notices.List(1)
		got := 0
		for _, x := range list.Notices {
			if x.ID == "cve-fixed" {
				got++
			}
		}
		if got != want {
			t.Errorf("build %s: %d, want %d", build, got, want)
		}
	}
}

func TestNoticesRefreshIsThrottled(t *testing.T) {
	r := newNoticeRig(t)
	r.rig.now = time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	r.source.publish(t, r.key)
	r.notices.Refresh()
	deadline := time.Now().Add(2 * time.Second)
	asked := func() int {
		r.source.mu.Lock()
		defer r.source.mu.Unlock()
		return r.source.requests
	}
	for r.notices.refreshing.Load() || asked() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the refresh never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.notices.Refresh()
	r.notices.Refresh()
	if r.notices.refreshing.Load() {
		t.Fatal("a fresh cache started another fetch")
	}
	if requests := asked(); requests != 1 {
		t.Fatalf("the sources were asked %d times within the TTL", requests)
	}
}
