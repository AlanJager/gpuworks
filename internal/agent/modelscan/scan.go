package modelscan

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

// Reasons a directory was looked at but is not offered as a model.
const (
	SkipNoWeights          = proto.ScanSkipNoWeights
	SkipEmpty              = proto.ScanSkipEmpty
	SkipMetadataOnly       = proto.ScanSkipMetadataOnly
	SkipHFHubCache         = proto.ScanSkipHFHubCache
	SkipTooDeep            = proto.ScanSkipTooDeep
	SkipUnreadable         = proto.ScanSkipUnreadable
	SkipUnrecognizedLayout = proto.ScanSkipUnrecognizedLayout
)

const (
	defaultMaxDepth = 4
	defaultLimit    = 200
)

// Candidate is one model directory found by Scan.
type Candidate struct {
	RelPath   string // slash-separated, relative to the scan root
	SizeBytes int64  // regular files, excluding dot directories and dot files
	Files     int
	Source    string
	SourceID  string
	Revision  string
	Subfolder string
	Basis     string
	Format    string
	Tasks     []string
	Issues    []modelmetadata.Issue

	Incomplete       bool
	IncompleteReason string
}

// Skipped is a directory that was examined and left out.
type Skipped struct{ RelPath, Reason string }

// Options tunes Scan. Zero values pick the defaults.
type Options struct {
	MaxDepth int       // default 4; the root's children are depth 1
	Limit    int       // max candidates per call, default 200
	Cursor   string    // resume after this RelPath (exclusive); "" starts over
	Deadline time.Time // zero means none

	trace func(op, rel string) // tests: every directory listed or parsed
	now   func() time.Time     // tests: the clock Deadline is read against
}

// Result is one page of a scan.
type Result struct {
	Candidates []Candidate
	Skipped    []Skipped
	NextCursor string // resume point when Complete is false
	Complete   bool
}

// errLeftRoot is a symlinked directory that no longer resolves inside the
// root. It is reported as any directory that cannot be read.
var errLeftRoot = errors.New("directory no longer resolves inside the scan root")

type node struct {
	rel     string
	depth   int
	scope   string
	context modelmetadata.Context
	// listLater marks a directory an earlier page reported whose children
	// are not needed until the walk reaches their level.
	listLater bool
	// viaLink marks a directory reached through a symlink, its own or an
	// ancestor's: it was inside the root when its parent was listed and is
	// checked again when it is read.
	viaLink bool
}

type scanner struct {
	root         string
	resolvedRoot string
	maxDepth     int
	cursor       string
	cursorDepth  int
	hasCursor    bool
	res          Result
	queue        []node
	limit        int
	full         bool
	// last is the furthest directory this walk has reported on, in scan
	// order: the resume point of the page being built.
	last string
	// born is when the walk's first page began (Pager's age limit).
	born  time.Time
	trace func(op, rel string)
	now   func() time.Time
}

// Scan walks root breadth first (siblings in name order) and returns the model
// entries identified by the metadata parser. Finding a model does not hide
// independent entries below it. Dot directories are never entered, and symlinked directories
// are followed only while they stay inside root.
//
// A call returns at most Options.Limit candidates; resume with NextCursor.
// Cancellation and the deadline end the call early with Complete=false and no
// error; only an unreadable root is an error. NextCursor is the last directory
// the call finished with, whether or not it was a model, so a page that found
// no model still moves the scan forward.
//
// Scan keeps nothing between calls: resuming lists again every directory
// above the cursor's level. A Pager resumes from where the last page stopped.
func Scan(ctx context.Context, root string, o Options) (Result, error) {
	return new(Pager).Scan(ctx, root, o)
}

func newScanner(root, resolved string, rootEntries []os.DirEntry, maxDepth int, cursor string) *scanner {
	s := &scanner{root: root, resolvedRoot: resolved, maxDepth: maxDepth}
	if cursor != "" {
		s.cursor, s.hasCursor, s.last = cursor, true, cursor
		s.cursorDepth = strings.Count(cursor, "/") + 1
	}
	ls := s.classifyEntries(root, rootEntries)
	for _, name := range ls.dirs {
		s.queue = append(s.queue, node{rel: name, depth: 1, viaLink: ls.links[name]})
	}
	return s
}

// page runs the walk until the page is full, the tree is exhausted or the
// call is interrupted. It reports whether the walk can be continued later:
// false when cancellation cut a directory short, which leaves the queue
// without that directory's children.
func (s *scanner) page(ctx context.Context, deadline time.Time) (resumable bool) {
	interrupted := false
	resumable = true
	for len(s.queue) > 0 && !s.full {
		if ctx.Err() != nil || (!deadline.IsZero() && !s.now().Before(deadline)) {
			interrupted = true
			break
		}
		n := s.queue[0]
		s.queue[0] = node{}
		s.queue = s.queue[1:]
		candidates, skipped := len(s.res.Candidates), len(s.res.Skipped)
		reported := s.visit(ctx, n)
		if ctx.Err() != nil {
			// A cancelled read looks like an unreadable directory. Nothing
			// about this directory is known: the next page reports it.
			s.res.Candidates, s.res.Skipped, s.full = s.res.Candidates[:candidates], s.res.Skipped[:skipped], false
			interrupted, resumable = true, false
			break
		}
		if reported {
			s.last = n.rel
		}
	}
	s.res.Complete = !interrupted && !s.full
	if !s.res.Complete {
		s.res.NextCursor = s.last
	}
	return resumable
}

// dirListing is a directory's non-dot content.
type dirListing struct {
	dirs  []string        // usable subdirectory names, in name order
	files []string        // regular files (and symlinks to them)
	links map[string]bool // the dirs that are symlinks
}

func (s *scanner) classifyEntries(dir string, entries []os.DirEntry) dirListing {
	var ls dirListing
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		switch {
		case e.IsDir():
			ls.dirs = append(ls.dirs, name)
		case e.Type().IsRegular():
			ls.files = append(ls.files, name)
		case e.Type()&fs.ModeSymlink != 0:
			path := filepath.Join(dir, name)
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.Mode().IsRegular() && s.insideRoot(path) {
				ls.files = append(ls.files, name)
			} else if info.IsDir() && s.insideRoot(path) {
				ls.dirs = append(ls.dirs, name)
				if ls.links == nil {
					ls.links = map[string]bool{}
				}
				ls.links[name] = true
			}
		}
	}
	return ls
}

func (s *scanner) insideRoot(path string) bool {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(s.resolvedRoot, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *scanner) skip(rel, reason string) {
	s.res.Skipped = append(s.res.Skipped, Skipped{RelPath: rel, Reason: reason})
}

func (s *scanner) note(op, rel string) {
	if s.trace != nil {
		s.trace(op, rel)
	}
}

// visit classifies one directory and reports whether it belongs to this
// page. Directories at or before the cursor were reported by an earlier page:
// nothing about them is reported again, and they are read only as far as
// their descendants need. One with no subdirectory to enter is not parsed,
// and one on the cursor's own level is not even listed until the walk
// reaches the level below it.
func (s *scanner) visit(ctx context.Context, n node) bool {
	done := n.listLater || (s.hasCursor && proto.ScanOrder(n.rel, s.cursor) <= 0)
	if done && !n.listLater && n.depth == s.cursorDepth {
		n.listLater = true
		s.queue = append(s.queue, n)
		return false
	}
	dir := filepath.Join(s.root, filepath.FromSlash(n.rel))
	s.note("list", n.rel)
	var entries []os.DirEntry
	err := errLeftRoot
	// The link was inside the root when its parent was listed. A walk
	// continued from an earlier page reads it much later, and whoever can
	// write to the share can have pointed it elsewhere since.
	if !n.viaLink || s.insideRoot(dir) {
		entries, err = os.ReadDir(dir)
	}
	if err != nil {
		if !done {
			s.skip(n.rel, SkipUnreadable)
		}
		return !done
	}
	ls := s.classifyEntries(dir, entries)
	if done && (len(ls.dirs) == 0 || n.depth >= s.maxDepth) {
		return false
	}

	s.note("parse", n.rel)
	report, err := s.parseNode(ctx, n)
	if err != nil {
		if !done {
			s.skip(n.rel, SkipUnreadable)
		}
		return !done
	}
	if report.Prune {
		if !done {
			s.skip(n.rel, SkipHFHubCache)
		}
		return !done
	}
	if !done {
		switch {
		case len(report.Entries) == 1:
			s.addCandidate(dir, n, report)
		case len(report.Entries) > 1:
			s.skip(n.rel, proto.ScanSkipMultipleEntries)
		case report.Component:
			s.skip(n.rel, proto.ScanSkipComponent)
		case slices.ContainsFunc(report.Issues, func(i modelmetadata.Issue) bool { return i.Code == "metadata_read_limit" }):
			s.skip(n.rel, proto.ScanSkipMetadataLimit)
		case report.HasMetadataIssues():
			if summarizeModelFiles(dir).size == 0 {
				s.skip(n.rel, SkipEmpty)
			} else {
				s.skip(n.rel, proto.ScanSkipInvalidMetadata)
			}
		}
	}
	// Context is reconstructed even for earlier pages. Its paths are relative
	// to scope, never to the storage root or a guessed directory depth.
	n.inheritMetadata(report)
	if len(ls.dirs) > 0 {
		if n.depth >= s.maxDepth {
			s.skip(n.rel, SkipTooDeep)
			return true
		}
		children := make([]node, 0, len(ls.dirs))
		for _, name := range ls.dirs {
			children = append(children, node{rel: n.rel + "/" + name, depth: n.depth + 1, scope: n.scope, context: n.context, viaLink: n.viaLink || ls.links[name]})
		}
		if n.listLater {
			// Its level is over: the children go ahead of those of the
			// directories that followed it there.
			s.queue = append(children, s.queue...)
		} else {
			s.queue = append(s.queue, children...)
		}
		return !done
	}
	if done || len(report.Entries) > 0 || report.Component || report.HasMetadataIssues() {
		return !done
	}
	switch {
	case len(ls.files) == 0:
		s.skip(n.rel, SkipEmpty)
	case allMetadata(ls.files):
		s.skip(n.rel, SkipMetadataOnly)
	default:
		s.skip(n.rel, SkipUnrecognizedLayout)
	}
	return true
}

func (s *scanner) addCandidate(dir string, n node, report modelmetadata.Report) {
	rel := n.rel
	in := summarizeModelFiles(dir)
	if in.unreadable {
		// Part of the model cannot be listed: its size and completeness are
		// unknown, so it is not offered as a model.
		s.skip(rel, SkipUnreadable)
		return
	}
	if in.size == 0 {
		// Files exist but hold no bytes: an unfinished or wiped download.
		s.skip(rel, SkipEmpty)
		return
	}
	c := Candidate{RelPath: rel, SizeBytes: in.size, Files: in.files}
	identify(dir, rel, &c)
	// A discovered subtree is registered by its physical directory. A remote
	// artifact subfolder preserves repository-relative paths, so inheriting the
	// parent's remote identity would bind a different on-disk layout.
	if n.scope != "" && n.scope != n.rel {
		c.Source, c.SourceID, c.Revision, c.Subfolder, c.Basis = SourceLocal, rel, "", "", BasisPath
	}

	e := report.Entries[0]
	c.Format, c.Tasks, c.Issues = e.Format, e.Tasks, report.Issues
	if e.Incomplete {
		c.Incomplete = true
		c.IncompleteReason = "model metadata could not be verified"
		if len(e.Missing) > 0 {
			c.IncompleteReason = "missing or unresolved files: " + strings.Join(e.Missing, ", ")
		}
	}
	if reason := in.reason(); reason != "" {
		c.Incomplete = true
		if c.IncompleteReason != "" {
			c.IncompleteReason += "; "
		}
		c.IncompleteReason += reason
	}
	s.res.Candidates = append(s.res.Candidates, c)
	if len(s.res.Candidates) >= s.limit {
		s.full = true
	}
}

func (s *scanner) parseNode(ctx context.Context, n node) (modelmetadata.Report, error) {
	scope := n.scope
	if scope == "" {
		scope = n.rel
	}
	v, err := OpenView(filepath.Join(s.root, filepath.FromSlash(scope)))
	if err != nil {
		return modelmetadata.Report{}, err
	}
	defer v.Close()
	name := "."
	if scope != n.rel {
		name = strings.TrimPrefix(n.rel, scope+"/")
	}
	return modelmetadata.Parse(ctx, modelmetadata.Input{Path: name, Context: n.context}, v)
}

// allMetadata reports whether every file is documentation or repo bookkeeping.
func allMetadata(files []string) bool {
	for _, f := range files {
		l := strings.ToLower(f)
		switch {
		case strings.HasSuffix(l, ".md"),
			strings.HasPrefix(l, "license"), strings.HasPrefix(l, "licence"),
			strings.HasPrefix(l, "notice"), strings.HasPrefix(l, "copying"),
			strings.HasPrefix(l, "citation"):
		default:
			return false
		}
	}
	return true
}
