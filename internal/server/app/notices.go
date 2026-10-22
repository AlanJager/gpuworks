package app

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// Notices are fetched from the distribution channel with the update check
// (ADR 0040: same sources, same occasions, same switch), and every
// noticesBackground while automatic checks are on, so an installation nobody
// signs in to still logs a security advisory that applies to it.
const (
	noticesTTL        = time.Hour
	noticesFailedTTL  = 5 * time.Minute
	noticesBackground = 6 * time.Hour
	noticesTimeout    = time.Minute
	// noticeVendorsTTL bounds how stale the fleet's vendors may be when
	// notices are matched; the bell asks on every console page.
	noticeVendorsTTL = time.Minute
)

// noticeService serves the console's notice bell.
type noticeService struct {
	store    *store.Store
	client   func() *distribution.Client
	settings *serverSettings
	log      *slog.Logger
	now      func() time.Time
	// started dates a notice that has no starts_at of its own.
	started time.Time

	refreshing atomic.Bool

	mu        sync.Mutex
	notices   []distribution.Notice
	fetchedAt time.Time
	failed    bool
	logged    map[string]bool
	vendors   []string
	vendorsAt time.Time
}

var _ api.Notices = (*noticeService)(nil)

// Refresh fetches the notices in the background unless they are fresh or a
// fetch is already running.
func (n *noticeService) Refresh() {
	n.mu.Lock()
	ttl := noticesTTL
	if n.failed {
		ttl = noticesFailedTTL
	}
	fresh := !n.fetchedAt.IsZero() && n.now().Sub(n.fetchedAt) < ttl
	n.mu.Unlock()
	if fresh || !n.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer n.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), noticesTimeout)
		defer cancel()
		n.fetch(ctx)
	}()
}

// Run refreshes the notices every noticesBackground while automatic update
// checks are on, until ctx ends.
func (n *noticeService) Run(ctx context.Context) {
	ticker := time.NewTicker(noticesBackground)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if auto, _ := n.settings.AutoCheckUpdates(); auto {
				n.Refresh()
			}
		}
	}
}

func (n *noticeService) fetch(ctx context.Context) {
	fetched, err := n.client().FetchNotices(ctx)
	now := n.now()
	n.mu.Lock()
	n.fetchedAt, n.failed = now, err != nil
	if err == nil {
		n.notices = fetched.Notices.Notices
	}
	n.mu.Unlock()
	if err != nil {
		n.log.Debug("notices not fetched", "err", err)
		return
	}
	keep := []string{}
	for _, x := range fetched.Notices.Notices {
		keep = append(keep, x.ID)
	}
	if err := n.store.DeleteNoticeReadsExcept(keep); err != nil {
		n.log.Debug("notices: prune read marks", "err", err)
	}
	n.logSecurity()
}

// logSecurity writes each security notice that applies here to the server
// log once, so log alerting catches it where nobody reads the console.
func (n *noticeService) logSecurity() {
	facts := n.facts()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.logged == nil {
		n.logged = map[string]bool{}
	}
	for _, x := range n.notices {
		if x.Kind != distribution.KindSecurity || n.logged[x.ID] || !x.Applies(facts) {
			continue
		}
		n.logged[x.ID] = true
		n.log.Warn("security notice from the publisher applies to this installation",
			"notice", x.ID, "severity", x.Severity, "title", x.Title["en"], "url", x.URL)
	}
}

func (n *noticeService) facts() distribution.NoticeFacts {
	channel, _ := n.settings.UpdateChannel()
	return distribution.NoticeFacts{Channel: channel, BuiltAt: version.BuildDate, Vendors: n.fleetVendors(), Now: n.now()}
}

// fleetVendors are the accelerator vendors of the fleet's nodes, read at
// most once per noticeVendorsTTL.
func (n *noticeService) fleetVendors() []string {
	n.mu.Lock()
	if !n.vendorsAt.IsZero() && n.now().Sub(n.vendorsAt) < noticeVendorsTTL {
		vendors := n.vendors
		n.mu.Unlock()
		return vendors
	}
	n.mu.Unlock()
	vendors, err := n.store.FleetVendors()
	if err != nil {
		n.log.Debug("notices: read the fleet's vendors", "err", err)
		return nil
	}
	n.mu.Lock()
	n.vendors, n.vendorsAt = vendors, n.now()
	n.mu.Unlock()
	return vendors
}

// severityRank orders the bell: most urgent first.
var severityRank = map[string]int{distribution.SeverityCritical: 0, distribution.SeverityImportant: 1, distribution.SeverityInfo: 2}

func (n *noticeService) List(userID uint) (dto.NoticeList, error) {
	reads, err := n.store.NoticeReads(userID)
	if err != nil {
		return dto.NoticeList{}, err
	}
	facts := n.facts()
	n.mu.Lock()
	notices := append([]distribution.Notice(nil), n.notices...)
	n.mu.Unlock()
	out := dto.NoticeList{Notices: []dto.Notice{}}
	for _, x := range notices {
		if !x.Applies(facts) {
			continue
		}
		published := x.StartsAt
		if published.IsZero() {
			published = n.started
		}
		out.Notices = append(out.Notices, dto.Notice{ID: x.ID, Severity: x.Severity, Kind: x.Kind, Title: x.Title, Body: x.Body,
			URL: x.URL, PublishedAt: published.UTC(), Read: reads[x.ID]})
	}
	sort.SliceStable(out.Notices, func(i, j int) bool {
		a, b := out.Notices[i], out.Notices[j]
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] < severityRank[b.Severity]
		}
		return a.PublishedAt.After(b.PublishedAt)
	})
	for _, x := range out.Notices {
		if !x.Read {
			out.Unread++
		}
	}
	return out, nil
}

func (n *noticeService) MarkRead(userID uint, ids []string) error {
	return n.store.MarkNoticesRead(userID, ids, n.now())
}
