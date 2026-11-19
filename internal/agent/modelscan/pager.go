package modelscan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A walk left unfinished is kept pagerHold for the next page, and no longer
// than pagerMaxAge after its first page however steadily it is paged: its
// queue is a listing of the tree as old as the walk, so a scan paged slowly
// does not go on reporting a tree that long gone. Past either, the next page
// walks from the root again and resumes after the cursor as any cold request
// does.
const (
	pagerHold   = 10 * time.Minute
	pagerMaxAge = 30 * time.Minute
)

// Pager scans roots page by page and keeps, per root, the walk the last
// unfinished page left behind. The page that resumes from that page's
// NextCursor continues the walk instead of repeating it, so the cost of a page
// does not grow with the part of the tree already scanned. Any other request
// (another cursor, a restart, a walk kept too long) walks from the root again
// and gives the same pages.
//
// The zero value is ready to use.
type Pager struct {
	mu   sync.Mutex
	held map[string]*heldWalk
	// now is the clock the two limits are read against; time.Now unless a
	// test sets it.
	now func() time.Time
}

type heldWalk struct {
	walk *scanner
	// born is when the walk's first page began, heldAt when its last page
	// ended.
	born, heldAt time.Time
	// timer frees a walk nobody came back for; take decides by the clock.
	timer *time.Timer
}

func (p *Pager) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Scan is modelscan.Scan, resuming a held walk when o.Cursor is where it
// stopped.
func (p *Pager) Scan(ctx context.Context, root string, o Options) (Result, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Result{}, err
	}
	rootEntries, err := os.ReadDir(resolved)
	if err != nil {
		return Result{}, err
	}
	maxDepth, limit := o.MaxDepth, o.Limit
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	cursor := strings.Trim(filepath.ToSlash(o.Cursor), "/")

	s := p.take(root, resolved, maxDepth, cursor)
	if s == nil {
		s = newScanner(root, resolved, rootEntries, maxDepth, cursor)
		s.born = p.clock()
	}
	s.res, s.full, s.limit, s.trace, s.now = Result{}, false, limit, o.trace, o.now
	if s.now == nil {
		s.now = time.Now
	}
	resumable := s.page(ctx, o.Deadline)
	res := s.res
	if resumable && !res.Complete && res.NextCursor != "" {
		p.hold(root, s)
	}
	return res, nil
}

// take removes and returns the walk held for root when it stopped exactly at
// cursor under the same depth. A walk past either limit, or one whose root
// has come to resolve elsewhere, is given up and answers nothing. An empty
// cursor always starts over.
func (p *Pager) take(root, resolved string, maxDepth int, cursor string) *scanner {
	now := p.clock()
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.held[root]
	if h == nil {
		return nil
	}
	if h.walk.resolvedRoot != resolved || now.Sub(h.heldAt) >= pagerHold || now.Sub(h.born) >= pagerMaxAge {
		h.timer.Stop()
		delete(p.held, root)
		return nil
	}
	if cursor == "" || h.walk.last != cursor || h.walk.maxDepth != maxDepth {
		return nil
	}
	h.timer.Stop()
	delete(p.held, root)
	return h.walk
}

func (p *Pager) hold(root string, s *scanner) {
	s.res, s.trace, s.now = Result{}, nil, nil
	// The part of the queue already walked is not kept alive with the rest.
	s.queue = append([]node(nil), s.queue...)
	h := &heldWalk{walk: s, born: s.born, heldAt: p.clock()}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil {
		p.held = map[string]*heldWalk{}
	}
	if old := p.held[root]; old != nil {
		old.timer.Stop()
	}
	p.held[root] = h
	h.timer = time.AfterFunc(pagerHold, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.held[root] == h {
			delete(p.held, root)
		}
	})
}

// Drop gives up the walk kept for root. The caller does so when what is at
// root may no longer be what was listed: its storage was unmounted, removed
// or defined anew.
func (p *Pager) Drop(root string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h := p.held[root]; h != nil {
		h.timer.Stop()
		delete(p.held, root)
	}
}

// holds is the cursor the walk kept for root resumes from, "" when none is
// kept.
func (p *Pager) holds(root string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h := p.held[root]; h != nil {
		return h.walk.last
	}
	return ""
}
