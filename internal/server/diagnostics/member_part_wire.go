package diagnostics

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// WriteMemberPart writes this member's part for the window as a gzipped
// tar: what another member asks for over the member-authenticated route.
// Nothing is written to this member's disk. It ends when ctx does, except
// inside a read the disk does not answer.
func WriteMemberPart(ctx context.Context, w io.Writer, src MemberSource, from, to time.Time) error {
	if !ValidWindow(from, to) {
		return errors.New("the window must end after it starts and be no longer than a collection's longest")
	}
	p, err := readMemberOwnPart(ctx, src, from, to, time.Now())
	if err != nil {
		return err
	}
	defer p.close()
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now()
	// member.json goes first: whatever is cut later, the asker knows who
	// answered and what was cut on this side.
	for _, f := range p.small() {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.data)), ModTime: now}); err != nil {
			return err
		}
		if _, err := tw.Write(f.data); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: MemberLogFile, Mode: 0o600, Size: p.logBytes, ModTime: now}); err != nil {
		return err
	}
	written, err := writeNewest(ctx, p.logs, tw, from, to, p.logBytes, p.logSkip)
	if err != nil {
		return err
	}
	// The generations only grow, so the second pass yields the lines the
	// first measured. A last line that was still being written then is
	// longer now and is left out: the entry is filled to its size.
	if short := p.logBytes - written; short > 0 {
		if _, err := io.CopyN(tw, newlines{}, short); err != nil {
			return err
		}
	}
	for _, f := range p.stalls {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := addFileToTar(tw, f.path, f.name); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

type newlines struct{}

func (newlines) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = '\n'
	}
	return len(p), nil
}

// partBudget is what may still be unpacked: of this member's part, and of
// every member's together.
type partBudget struct {
	member int64
	bundle *atomic.Int64
}

// take grants up to n bytes.
func (b *partBudget) take(n int64) int64 {
	if n > b.member {
		n = b.member
	}
	if n <= 0 {
		return 0
	}
	if left := b.bundle.Add(-n); left < 0 {
		// Give back what the bundle did not have.
		over := -left
		if over > n {
			over = n
		}
		b.bundle.Add(over)
		n -= over
	}
	b.member -= n
	return n
}

// budgetWriter writes while the budget lasts.
type budgetWriter struct {
	w      io.Writer
	budget *partBudget
	spent  bool
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	granted := b.budget.take(int64(len(p)))
	n, err := b.w.Write(p[:granted])
	if err == nil && granted < int64(len(p)) {
		b.spent = true
		err = errPartBudget
	}
	return n, err
}

var errPartBudget = errors.New("the size bound of the members' parts was reached")

// readMemberPart unpacks another member's part into dir. Only the files a
// part is made of are taken, each up to its bound, stall reports up to
// their count, all within the budget; anything else in the stream is
// ignored, so a member cannot place a file elsewhere or fill the disk. A
// part that reaches the budget is kept as far as it went, its last file
// marked truncated, and answered with errPartBudget: it is not whole.
func readMemberPart(r io.Reader, dir string, budget *partBudget) ([]memberFile, error) {
	gz, err := gzip.NewReader(io.LimitReader(r, memberBounds.part))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	if err := os.MkdirAll(filepath.Join(dir, "stalls"), 0o700); err != nil {
		return nil, err
	}
	var files []memberFile
	seen := map[string]bool{}
	stalls := 0
	done := func(err error) ([]memberFile, error) {
		sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
		if err == nil && !seen[MemberFactsFile] {
			err = errors.New("the member's part holds no member.json")
		}
		return files, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return done(nil)
		}
		if err != nil {
			return done(fmt.Errorf("the member's part ended early: %w", err))
		}
		name := path.Clean(h.Name)
		limit, ok := memberFileBound(name)
		if !ok || seen[name] || h.Typeflag != tar.TypeReg {
			continue
		}
		if strings.HasPrefix(name, memberStallPrefix) {
			if stalls == memberBounds.stalls {
				continue
			}
			stalls++
		}
		seen[name] = true
		local := filepath.Join(dir, filepath.FromSlash(name))
		f, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return done(err)
		}
		out := &budgetWriter{w: f, budget: budget}
		n, err := io.Copy(out, io.LimitReader(tr, limit))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		// A file cut off by the stream's end is not whole either.
		files = append(files, memberFile{name: name, path: local, bytes: n, truncated: h.Size > limit || err != nil})
		if out.spent {
			// What follows in the part is not taken: the part is not whole,
			// and must not be recorded as collected.
			return done(errPartBudget)
		}
		if err != nil {
			return done(fmt.Errorf("the member's part ended early: %w", err))
		}
	}
}

// memberFileBound is the size a part's file may have; ok is false for a
// name that is not a part's.
func memberFileBound(name string) (int64, bool) {
	switch name {
	case MemberLogFile:
		return memberBounds.log, true
	case MemberFactsFile, MemberRoleEventsFile, MemberLeaderChangesFile, MemberStatusFile:
		return memberBounds.small, true
	}
	if base, ok := strings.CutPrefix(name, memberStallPrefix); ok && base != "" && !strings.Contains(base, "/") &&
		strings.HasPrefix(base, "stall-") && strings.HasSuffix(base, ".txt") {
		return memberBounds.small, true
	}
	return 0, false
}

// readMemberFacts reads an unpacked part's member.json.
func readMemberFacts(dir string) (MemberFacts, error) {
	raw, err := os.ReadFile(filepath.Join(dir, MemberFactsFile))
	if err != nil {
		return MemberFacts{}, err
	}
	var facts MemberFacts
	if err := json.Unmarshal(raw, &facts); err != nil {
		return MemberFacts{}, err
	}
	return facts, nil
}
