package modelscan

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// listBudget makes Options whose deadline falls after a page listed budget
// directories, whatever the machine's speed: the clock moves only when the
// scan lists a directory. seen counts every list and parse by path.
type listBudget struct {
	ticks int
	seen  map[string]int
}

func (b *listBudget) options(budget int, o Options) Options {
	start := time.Unix(0, 0).Add(time.Duration(b.ticks) * time.Second)
	o.Deadline = start.Add(time.Duration(budget) * time.Second)
	o.now = func() time.Time { return time.Unix(0, 0).Add(time.Duration(b.ticks) * time.Second) }
	o.trace = func(op, rel string) {
		if op == "list" {
			b.ticks++
		}
		if b.seen == nil {
			b.seen = map[string]int{}
		}
		b.seen[op+" "+rel]++
	}
	return o
}

// noiseLibrary is the shape that stalled a large share: many directories that
// hold no model ahead of the one that does.
func noiseLibrary(t *testing.T, orgs int) string {
	t.Helper()
	root := t.TempDir()
	for i := range orgs {
		write(t, p(root, fmt.Sprintf("org%04d/pkg/notes.txt", i)), "x")
	}
	hfModel(t, root, "zzz/real-model")
	return root
}

// drain pages through scan until it completes and checks every page moved
// strictly forward and reported nothing twice.
func drain(t *testing.T, scan func(cursor string) Result) (candidates []string, skipped map[string]string, pages int) {
	t.Helper()
	skipped = map[string]string{}
	cursor := ""
	for pages = 1; ; pages++ {
		if pages > 10000 {
			t.Fatalf("scan did not complete; cursor %q", cursor)
		}
		res := scan(cursor)
		for _, c := range res.Candidates {
			if slices.Contains(candidates, c.RelPath) {
				t.Fatalf("page %d: candidate %s reported twice", pages, c.RelPath)
			}
			candidates = append(candidates, c.RelPath)
		}
		for _, s := range res.Skipped {
			if _, dup := skipped[s.RelPath+" "+s.Reason]; dup {
				t.Fatalf("page %d: skip %+v reported twice", pages, s)
			}
			skipped[s.RelPath+" "+s.Reason] = s.Reason
			if cursor != "" && proto.ScanOrder(s.RelPath, cursor) <= 0 {
				t.Fatalf("page %d from %q reported %s, which an earlier page covers", pages, cursor, s.RelPath)
			}
			if !res.Complete && proto.ScanOrder(s.RelPath, res.NextCursor) > 0 {
				t.Fatalf("page %d ends at %q but reported %s beyond it", pages, res.NextCursor, s.RelPath)
			}
		}
		if res.Complete {
			if res.NextCursor != "" {
				t.Fatalf("complete page carries cursor %q", res.NextCursor)
			}
			return candidates, skipped, pages
		}
		if res.NextCursor == "" || (cursor != "" && proto.ScanOrder(res.NextCursor, cursor) <= 0) {
			t.Fatalf("page %d did not advance: %q -> %q (candidates=%d skipped=%d)", pages, cursor, res.NextCursor, len(res.Candidates), len(res.Skipped))
		}
		cursor = res.NextCursor
	}
}

func scanAll(t *testing.T, root string) ([]string, map[string]string, *listBudget) {
	t.Helper()
	b := &listBudget{}
	res, err := Scan(context.Background(), root, b.options(1<<30, Options{Limit: 1 << 30}))
	if err != nil || !res.Complete {
		t.Fatalf("full scan: %+v err=%v", res, err)
	}
	skipped := map[string]string{}
	for _, s := range res.Skipped {
		skipped[s.RelPath+" "+s.Reason] = s.Reason
	}
	return slices.Collect(maps.Keys(byRel(res))), skipped, b
}

// A page cut short before it found any model still ends further on, and its
// skipped directories are reported once: the scan of a share where models are
// rare reaches the end.
func TestScanAdvancesThroughPagesThatFindNoModel(t *testing.T) {
	root := noiseLibrary(t, 300)
	wantCandidates, wantSkipped, whole := scanAll(t, root)

	var pager Pager
	b := &listBudget{}
	candidates, skipped, pages := drain(t, func(cursor string) Result {
		res, err := pager.Scan(context.Background(), root, b.options(40, Options{Cursor: cursor, Limit: 100}))
		if err != nil {
			t.Fatal(err)
		}
		return res
	})
	if !slices.Equal(candidates, []string{"zzz/real-model"}) || !slices.Equal(wantCandidates, candidates) {
		t.Fatalf("candidates = %v, full scan %v", candidates, wantCandidates)
	}
	if !maps.Equal(skipped, wantSkipped) || len(skipped) != 300 {
		t.Fatalf("paged scan skipped %d directories, full scan %d", len(skipped), len(wantSkipped))
	}
	if pages < 10 {
		t.Fatalf("the budget was meant to cut the scan into many pages, got %d", pages)
	}
	// Continuing costs what is left, not what is behind: across all pages no
	// directory was listed or parsed more often than by one uninterrupted scan.
	if !maps.Equal(b.seen, whole.seen) {
		for k, n := range b.seen {
			if n != whole.seen[k] {
				t.Fatalf("%s: %d times over the pages, %d in one scan", k, n, whole.seen[k])
			}
		}
		t.Fatalf("pages touched %d directories, one scan %d", len(b.seen), len(whole.seen))
	}
}

// Without the walk of the page before (another agent process, or the walk
// expired), a page first gets back to the cursor. That may take the whole
// page; the pager then continues from there on the retry instead of starting
// over, so the same cursor is returned at most while the way back is walked.
func TestScanResumesAColdCursorOverSeveralPages(t *testing.T) {
	root := noiseLibrary(t, 300)
	cursor := "org0150/pkg"
	var pager Pager
	b := &listBudget{}
	for page := 1; ; page++ {
		if page > 20 {
			t.Fatalf("still at %q after %d pages", cursor, page)
		}
		res, err := pager.Scan(context.Background(), root, b.options(100, Options{Cursor: cursor}))
		if err != nil || res.Complete {
			t.Fatalf("page %d: %+v err=%v", page, res, err)
		}
		if res.NextCursor != cursor {
			if proto.ScanOrder(res.NextCursor, cursor) <= 0 || len(res.Skipped) == 0 || res.Skipped[0].RelPath != "org0151/pkg" {
				t.Fatalf("page %d moved to %q with %+v", page, res.NextCursor, res.Skipped)
			}
			break
		}
		if len(res.Candidates)+len(res.Skipped) != 0 {
			t.Fatalf("page %d stayed at the cursor but reported %+v", page, res)
		}
	}
	for k, n := range b.seen {
		if n > 1 {
			t.Fatalf("%s done %d times while walking back to the cursor", k, n)
		}
	}
}

// Walking back to a cursor reads only what the directories after it need:
// a directory already reported is parsed only when it has subdirectories
// (they inherit from it), and one on the cursor's level is not listed before
// the walk reaches the level below.
func TestScanDoesNotParseReportedDirectoriesAgain(t *testing.T) {
	root := t.TempDir()
	for _, org := range []string{"a", "b", "c"} {
		for _, m := range []string{"m1", "m2", "m3"} {
			hfModel(t, root, org+"/"+m)
		}
	}
	mkdir(t, p(root, "a/m2/onnx"))
	write(t, p(root, "a/m2/onnx/model.onnx"), "onnx")

	b := &listBudget{}
	res, err := Scan(context.Background(), root, b.options(5, Options{Cursor: "b/m2"}))
	if err != nil || res.Complete || res.NextCursor != "c/m1" {
		t.Fatalf("page: %+v err=%v", res, err)
	}
	if got := slices.Sorted(maps.Keys(byRel(res))); !slices.Equal(got, []string{"b/m3", "c/m1"}) {
		t.Fatalf("candidates = %v", got)
	}
	want := map[string]int{
		"list a": 1, "parse a": 1, "list b": 1, "parse b": 1, "list c": 1, "parse c": 1,
		"list b/m3": 1, "parse b/m3": 1, "list c/m1": 1, "parse c/m1": 1,
	}
	if !maps.Equal(b.seen, want) {
		t.Fatalf("touched %v, want %v", b.seen, want)
	}

	// To the end: the reported level is listed once the next one is due, and
	// only a/m2, which has a subdirectory, is parsed again.
	b = &listBudget{}
	res, err = Scan(context.Background(), root, b.options(1<<30, Options{Cursor: "b/m2"}))
	if err != nil || !res.Complete {
		t.Fatalf("rest: %+v err=%v", res, err)
	}
	for _, rel := range []string{"a/m1", "a/m3", "b/m1", "b/m2"} {
		if b.seen["parse "+rel] != 0 || b.seen["list "+rel] != 1 {
			t.Fatalf("%s: listed %d, parsed %d", rel, b.seen["list "+rel], b.seen["parse "+rel])
		}
	}
	if b.seen["parse a/m2"] != 1 || b.seen["list a/m2/onnx"] != 1 {
		t.Fatalf("a/m2 and its subdirectory: %v", b.seen)
	}
}

// Every way of cutting a library into pages reports exactly what one scan
// does, in the same order: by candidate limit, by deadline with the walk
// kept, and by deadline without it.
func TestScanPagesAgreeWithOneScan(t *testing.T) {
	root := buildLibrary(t)
	hfModel(t, root, "nested/outer")
	hfModel(t, root, "nested/outer/inner/model")
	mkdir(t, p(root, "deep/a/b/c/d/e"))
	if err := os.Symlink(p(root, "Qwen"), p(root, "link-to-qwen")); err != nil {
		t.Fatal(err)
	}
	mkdir(t, p(root, ".hidden/model"))
	wantCandidates, wantSkipped, _ := scanAll(t, root)
	slices.SortFunc(wantCandidates, proto.ScanOrder)

	check := func(name string, scan func(cursor string) Result) {
		t.Helper()
		candidates, skipped, pages := drain(t, scan)
		if !slices.Equal(candidates, wantCandidates) {
			t.Fatalf("%s: candidates over %d pages = %v, want %v", name, pages, candidates, wantCandidates)
		}
		if !maps.Equal(skipped, wantSkipped) {
			t.Fatalf("%s: skipped = %v, want %v", name, skipped, wantSkipped)
		}
	}
	page := func(scan func(context.Context, string, Options) (Result, error), o func(cursor string) Options) func(string) Result {
		return func(cursor string) Result {
			res, err := scan(context.Background(), root, o(cursor))
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
	}
	check("limit 1, stateless", page(Scan, func(c string) Options { return Options{Cursor: c, Limit: 1} }))
	var byLimit Pager
	check("limit 1, kept walk", page(byLimit.Scan, func(c string) Options { return Options{Cursor: c, Limit: 1} }))
	for _, budget := range []int{1, 2, 3, 7} {
		var kept Pager
		b := &listBudget{}
		check(fmt.Sprintf("budget %d, kept walk", budget), page(kept.Scan, func(c string) Options { return b.options(budget, Options{Cursor: c}) }))
	}
	// Stateless pages need a budget that gets them back to the cursor.
	b := &listBudget{}
	check("budget 60, stateless", page(Scan, func(c string) Options { return b.options(60, Options{Cursor: c, Limit: 2}) }))
}

// Cancellation in the middle of a directory must not report that directory
// as unreadable nor move past it: the next page reports it for what it is.
func TestScanCancelledMidDirectoryLeavesItToTheNextPage(t *testing.T) {
	root := t.TempDir()
	hfModel(t, root, "a/m1")
	hfModel(t, root, "a/m2")
	hfModel(t, root, "a/m3")

	var pager Pager
	ctx, cancel := context.WithCancel(context.Background())
	res, err := pager.Scan(ctx, root, Options{trace: func(op, rel string) {
		if op == "parse" && rel == "a/m2" {
			cancel()
		}
	}})
	if err != nil || res.Complete || res.NextCursor != "a/m1" || len(res.Skipped) != 0 {
		t.Fatalf("cancelled page: %+v err=%v", res, err)
	}
	if got := slices.Collect(maps.Keys(byRel(res))); !slices.Equal(got, []string{"a/m1"}) {
		t.Fatalf("cancelled page candidates = %v", got)
	}
	res, err = pager.Scan(context.Background(), root, Options{Cursor: res.NextCursor})
	if err != nil || !res.Complete || len(res.Skipped) != 0 {
		t.Fatalf("next page: %+v err=%v", res, err)
	}
	if got := slices.Sorted(maps.Keys(byRel(res))); !slices.Equal(got, []string{"a/m2", "a/m3"}) {
		t.Fatalf("next page candidates = %v", got)
	}
}

// A kept walk serves only the page that follows it: a new scan, another
// cursor, another depth or an expired walk start from the root.
func TestPagerKeepsAWalkOnlyForItsOwnNextPage(t *testing.T) {
	root := t.TempDir()
	for _, m := range []string{"m1", "m2", "m3", "m4"} {
		hfModel(t, root, "a/"+m)
	}
	var pager Pager
	first, err := pager.Scan(context.Background(), root, Options{Limit: 1})
	if err != nil || first.NextCursor != "a/m1" {
		t.Fatalf("first: %+v err=%v", first, err)
	}
	// A model that appears after the walk listed its level is found by a
	// scan that starts over, and by a page the walk was not kept for.
	hfModel(t, root, "a/m0")
	fresh, err := pager.Scan(context.Background(), root, Options{Limit: 1})
	if err != nil || fresh.NextCursor != "a/m0" {
		t.Fatalf("a new scan reused the old walk: %+v err=%v", fresh, err)
	}
	other, err := pager.Scan(context.Background(), root, Options{Cursor: "a/m2", MaxDepth: 3})
	if err != nil || !other.Complete || len(other.Candidates) != 2 {
		t.Fatalf("another cursor: %+v err=%v", other, err)
	}
	if len(pager.held) != 1 || pager.held[root].walk.last != "a/m0" {
		t.Fatalf("held walks = %+v", pager.held)
	}
	rest, err := pager.Scan(context.Background(), root, Options{Cursor: "a/m0"})
	if err != nil || !rest.Complete || len(rest.Candidates) != 4 || len(pager.held) != 0 {
		t.Fatalf("rest: %+v err=%v held=%d", rest, err, len(pager.held))
	}
}

// pagedClock is a Pager's clock a test moves by hand.
type pagedClock struct{ at time.Time }

func (c *pagedClock) now() time.Time { return c.at }

// A kept walk is a listing as old as its first page. It is given up when
// nobody came back for it in pagerHold, and pagerMaxAge after that first page
// however steadily it is paged; the page that follows then walks from the
// root and still resumes after the cursor, finding what the old listing never
// had.
func TestPagerGivesUpAWalkThatIsTooOld(t *testing.T) {
	for name, tc := range map[string]struct {
		// waits are the clock steps before each page after the first.
		waits     []time.Duration
		wantFresh int // the page (1-based, after the first) that must walk anew
	}{
		"left for longer than the hold":   {[]time.Duration{pagerHold}, 1},
		"paged steadily past the max age": {[]time.Duration{pagerHold - time.Minute, pagerHold - time.Minute, pagerHold - time.Minute, pagerHold - time.Minute}, 4},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for _, m := range []string{"m1", "m2", "m3", "m4", "m5", "m6"} {
				hfModel(t, root, "a/"+m)
			}
			clock := &pagedClock{at: time.Unix(1_700_000_000, 0)}
			pager := Pager{now: clock.now}
			res, err := pager.Scan(context.Background(), root, Options{Limit: 1})
			if err != nil || res.NextCursor != "a/m1" || pager.holds(root) != "a/m1" {
				t.Fatalf("first: %+v err=%v", res, err)
			}
			// A model the kept listing does not know: only a walk from the
			// root finds it.
			hfModel(t, root, "a/m7")
			found := []string{"a/m1"}
			for i, wait := range tc.waits {
				clock.at = clock.at.Add(wait)
				lists := 0
				res, err = pager.Scan(context.Background(), root, Options{Limit: 1, Cursor: res.NextCursor, trace: func(op, rel string) {
					if op == "list" && rel == "a" {
						lists++
					}
				}})
				if err != nil || len(res.Candidates) != 1 {
					t.Fatalf("page %d: %+v err=%v", i+2, res, err)
				}
				found = append(found, res.Candidates[0].RelPath)
				if fresh := lists > 0; fresh != (i+1 == tc.wantFresh) {
					t.Fatalf("page %d walked from the root = %v, want %v", i+2, fresh, i+1 == tc.wantFresh)
				}
			}
			rest, err := pager.Scan(context.Background(), root, Options{Cursor: res.NextCursor})
			if err != nil || !rest.Complete {
				t.Fatalf("rest: %+v err=%v", rest, err)
			}
			for _, c := range rest.Candidates {
				found = append(found, c.RelPath)
			}
			if want := []string{"a/m1", "a/m2", "a/m3", "a/m4", "a/m5", "a/m6", "a/m7"}; !slices.Equal(found, want) {
				t.Fatalf("found %v, want %v with nothing twice", found, want)
			}
		})
	}
}

// A root that has come to resolve to another directory (a mount point
// remounted, a link repointed) is another tree: the walk kept for the old one
// is not continued over it.
func TestPagerDoesNotContinueOverARootThatMoved(t *testing.T) {
	base := t.TempDir()
	for _, m := range []string{"m1", "m2", "m3"} {
		hfModel(t, p(base, "old"), "a/"+m)
	}
	for _, m := range []string{"m1", "n2"} {
		hfModel(t, p(base, "new"), "a/"+m)
	}
	root := p(base, "mount")
	if err := os.Symlink(p(base, "old"), root); err != nil {
		t.Fatal(err)
	}
	var pager Pager
	first, err := pager.Scan(context.Background(), root, Options{Limit: 1})
	if err != nil || first.NextCursor != "a/m1" || pager.holds(root) != "a/m1" {
		t.Fatalf("first: %+v err=%v", first, err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(p(base, "new"), root); err != nil {
		t.Fatal(err)
	}
	rest, err := pager.Scan(context.Background(), root, Options{Cursor: "a/m1"})
	if err != nil || !rest.Complete || len(rest.Candidates) != 1 || rest.Candidates[0].RelPath != "a/n2" || len(rest.Skipped) != 0 {
		t.Fatalf("rest = %+v err=%v, want the new tree's a/n2 and nothing of the old one", rest, err)
	}
	if pager.holds(root) != "" {
		t.Fatal("the old root's walk is still kept")
	}
}

// Drop gives up the kept walk: the next page walks from the root.
func TestPagerDropGivesUpTheKeptWalk(t *testing.T) {
	root := t.TempDir()
	for _, m := range []string{"m1", "m2"} {
		hfModel(t, root, "a/"+m)
	}
	var pager Pager
	if first, err := pager.Scan(context.Background(), root, Options{Limit: 1}); err != nil || pager.holds(root) != first.NextCursor || first.NextCursor == "" {
		t.Fatalf("first: %+v err=%v", first, err)
	}
	pager.Drop(root)
	pager.Drop(p(root, "never-scanned"))
	if pager.holds(root) != "" {
		t.Fatal("the walk is still kept")
	}
}

// A symlinked directory was inside the root when its parent was listed. A
// walk continued from an earlier page reads it later; pointed outside the
// root in between, it is not entered.
func TestKeptWalkDoesNotFollowALinkThatLeftTheRoot(t *testing.T) {
	base := t.TempDir()
	root := p(base, "share")
	hfModel(t, root, "a/m1")
	hfModel(t, root, "inside/m2")
	// The same name outside the root: what the link's queued child would
	// read if the link were followed again without a check.
	hfModel(t, p(base, "outside"), "m2")
	if err := os.Symlink(p(root, "inside"), p(root, "link")); err != nil {
		t.Fatal(err)
	}
	var pager Pager
	first, err := pager.Scan(context.Background(), root, Options{Limit: 1})
	if err != nil || len(first.Candidates) != 1 || pager.holds(root) == "" {
		t.Fatalf("first: %+v err=%v", first, err)
	}
	if err := os.Remove(p(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(p(base, "outside"), p(root, "link")); err != nil {
		t.Fatal(err)
	}
	rest, err := pager.Scan(context.Background(), root, Options{Cursor: first.NextCursor})
	if err != nil || !rest.Complete {
		t.Fatalf("rest: %+v err=%v", rest, err)
	}
	for _, c := range rest.Candidates {
		if c.RelPath == "link/m2" {
			t.Fatalf("the walk entered a link that left the root: %+v", rest.Candidates)
		}
	}
	if !slices.Contains(rest.Skipped, Skipped{RelPath: "link/m2", Reason: SkipUnreadable}) {
		t.Fatalf("skipped = %+v, want the directory under the link reported as not readable", rest.Skipped)
	}
}
