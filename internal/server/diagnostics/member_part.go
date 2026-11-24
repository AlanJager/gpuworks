package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/stallwatch"
)

// A control-plane cluster keeps each member's log, stall reports and
// database role records on that member. Explaining what the cluster did (a
// leader change above all) needs every member's, so each member can hand
// out its own part: the files below, read from its directories alone. No
// database is asked, so a member answers while it has no leader, while its
// business is closed and while its server is ahead of the schema.

// memberBounds are the bounds of a member's part. Every side reads them
// here: the member that reads its part, the member that unpacks it, the
// asker's wait and the route's deadline. Tests lower them.
var memberBounds = struct {
	// log bounds a member's log in a part, small each of its other files.
	log, small int64
	// stalls and stallBytes bound the stall reports of a part; the newest
	// are kept.
	stalls     int
	stallBytes int64
	// part bounds one member's part as it is unpacked, all every member's
	// together.
	part, all int64
	// wait bounds one member's answer. busyWait is how long a member that
	// is giving its part to another collection is left before it is asked
	// once more.
	wait, busyWait time.Duration
}{
	log: 64 << 20, small: 8 << 20,
	stalls: 64, stallBytes: 32 << 20,
	part: 128 << 20, all: 512 << 20,
	wait: 2 * time.Minute, busyWait: 3 * time.Second,
}

// MemberPartTimeout bounds one member's answer: the asker waits this long,
// and the route that gives a part writes it within it.
func MemberPartTimeout() time.Duration { return memberBounds.wait }

// The files of a member's part, relative to its directory in a collection.
const (
	MemberFactsFile         = "member.json"
	MemberLogFile           = "server.log"
	MemberRoleEventsFile    = "role-events.log"
	MemberLeaderChangesFile = "leader-changes.log"
	MemberStatusFile        = "cluster-status.txt"
	memberStallPrefix       = "stalls/"
)

// MemberFacts is a part's member.json: who the member is and what it runs.
type MemberFacts struct {
	ID      uint64 `json:"id"`
	Address string `json:"address"`
	Version string `json:"server_version"`
	// ServerSHA256 and DatabaseProcessSHA256 are the running executables'
	// digests; empty when one could not be read.
	ServerSHA256          string `json:"server_sha256,omitempty"`
	DatabaseProcessSHA256 string `json:"database_process_sha256,omitempty"`
	StorageTier           string `json:"storage_tier,omitempty"`
	// LogFile is where the member's server log is on its host.
	LogFile string `json:"log_file,omitempty"`
	// CollectedAt is the member's own clock when it built the part. Hosts'
	// clocks differ: members/index.json sets it against the collecting
	// member's clock, so the members' lines can be put on one timeline.
	CollectedAt time.Time `json:"collected_at"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	// Truncated names the files that reached their size bound.
	Truncated []string `json:"truncated"`
}

// MemberSource is what a member's part is read from (app wires it).
type MemberSource interface {
	// Facts are the member's identity and executables.
	Facts(ctx context.Context) MemberFacts
	// LogFiles are the server's log file and its rotated generations.
	LogFiles() []string
	// StallDir is where the server keeps its stall reports.
	StallDir() string
	// Status is the cluster as this member observes it now, as the status
	// report words it; empty when it could not be observed.
	Status(ctx context.Context) string
}

// memberFile is one file of a part in a collection's staging directory.
type memberFile struct {
	name, path string
	bytes      int64
	truncated  bool
}

// roleEventLine and leaderChangeLine pick the database role records out of a
// server log (internal/server/cluster/role_events.go and the business
// generation's own line).
func roleEventLine(line []byte) bool {
	return bytes.Contains(line, []byte(`msg="cluster database role event`))
}

func leaderChangeLine(line []byte) bool {
	return bytes.Contains(line, []byte(`msg="cluster database leader changed"`)) ||
		bytes.Contains(line, []byte(`msg="cluster database leader kept after a campaign"`)) ||
		bytes.Contains(line, []byte(`msg="business follows a new database leader"`))
}

// ctxReader ends a read loop when its context does. A read already inside
// the kernel (a disk that does not answer) is not interrupted.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// capped keeps the newest of the lines it is given that fit its bound.
type capped struct {
	data      []byte
	truncated bool
}

func (c *capped) add(line []byte) {
	c.data = append(c.data, line...)
	for int64(len(c.data)) > memberBounds.small {
		c.truncated = true
		end := bytes.IndexByte(c.data, '\n')
		if end < 0 {
			c.data = nil
			return
		}
		c.data = c.data[end+1:]
	}
}

// recordSink keeps the role records of the log lines it is given, one Write
// a line (filterLogWindow writes so).
type recordSink struct {
	roles, leaders capped
}

func (s *recordSink) Write(line []byte) (int, error) {
	switch {
	case roleEventLine(line):
		s.roles.add(line)
	case leaderChangeLine(line):
		s.leaders.add(line)
	}
	return len(line), nil
}

// memberPart is a member's part, read and measured, before it is written.
// Nothing is staged on disk: the log's generations stay open between the
// pass that measures the window and the pass that writes it, so a rotation
// in between changes nothing, and the small files are held in memory.
type memberPart struct {
	facts MemberFacts
	logs  []*os.File // oldest first
	// logBytes of the window's newest lines are given, after logSkip bytes
	// of its oldest that did not fit.
	logBytes, logSkip int64
	roles, leaders    []byte
	status            string
	stalls            []memberFile
}

// newMemberPart opens the member's log for the window. The caller closes
// the part.
func newMemberPart(ctx context.Context, src MemberSource, from, to, now time.Time) *memberPart {
	p := &memberPart{facts: src.Facts(ctx), logs: openLogWindow(src.LogFiles(), from)}
	p.facts.CollectedAt, p.facts.From, p.facts.To, p.facts.Truncated = now, from, to, []string{}
	return p
}

func (p *memberPart) close() { closeFiles(p.logs) }

func closeFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// openLogWindow opens the generations (given newest first) that can hold a
// line of the window, oldest first.
func openLogWindow(files []string, from time.Time) []*os.File {
	var out []*os.File
	for i := len(files) - 1; i >= 0; i-- {
		info, err := os.Stat(files[i])
		if err != nil || info.ModTime().Before(from) {
			continue // a generation last written before the window holds nothing in it
		}
		if f, err := os.Open(files[i]); err == nil {
			out = append(out, f)
		}
	}
	return out
}

// filterFiles writes the lines of the opened generations that lie in the
// window, each read from its start.
func filterFiles(ctx context.Context, files []*os.File, w io.Writer, from, to time.Time, max int64) (int64, bool, error) {
	var written int64
	for _, f := range files {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return written, false, err
		}
		n, truncated, err := filterLogWindow(ctxReader{ctx, f}, w, from, to, max-written)
		written += n
		if err != nil || truncated {
			return written, truncated, err
		}
	}
	return written, false, nil
}

// dropOldest leaves out whole lines from the start of what it is given
// until need bytes are gone, and passes the rest on when out is set.
type dropOldest struct {
	out           io.Writer
	need, dropped int64
}

func (d *dropOldest) Write(line []byte) (int, error) {
	if d.dropped < d.need {
		d.dropped += int64(len(line))
		return len(line), nil
	}
	if d.out != nil {
		return d.out.Write(line)
	}
	return len(line), nil
}

// measureNewest reads the window's lines of the opened generations into
// sink (all of them, whatever the bound) and answers how much of the window
// fits max when its oldest lines are left out: kept bytes after skip bytes.
// A log cut at its bound keeps its newest lines, which are the ones a
// collection is made for.
func measureNewest(ctx context.Context, files []*os.File, sink io.Writer, from, to time.Time, max int64) (kept, skip int64, err error) {
	total, _, err := filterFiles(ctx, files, sink, from, to, math.MaxInt64)
	if err != nil || total <= max {
		return total, 0, err
	}
	oldest := &dropOldest{need: total - max}
	if _, _, err := filterFiles(ctx, files, oldest, from, to, total); err != nil {
		return 0, 0, err
	}
	return total - oldest.dropped, oldest.dropped, nil
}

// writeNewest writes what measureNewest measured: the window's lines after
// the first skip bytes, up to kept bytes. The generations only grow, so the
// lines are the ones that were measured.
func writeNewest(ctx context.Context, files []*os.File, w io.Writer, from, to time.Time, kept, skip int64) (int64, error) {
	oldest := &dropOldest{out: w, need: skip}
	n, _, err := filterFiles(ctx, files, oldest, from, to, skip+kept)
	return n - oldest.dropped, err
}

// logWindow writes the lines of a log's generations (given newest first)
// that lie in the window, oldest first; when they exceed max the newest are
// kept.
func logWindow(ctx context.Context, files []string, w io.Writer, from, to time.Time, max int64) (int64, bool, error) {
	opened := openLogWindow(files, from)
	defer closeFiles(opened)
	kept, skip, err := measureNewest(ctx, opened, io.Discard, from, to, max)
	if err != nil {
		return 0, false, err
	}
	n, err := writeNewest(ctx, opened, w, from, to, kept, skip)
	return n, skip > 0, err
}

// memberStalls are the stall reports of the window that fit the bounds,
// newest kept.
func memberStalls(dir string, from, to time.Time) (kept []memberFile, truncated bool) {
	reports := stallwatch.Reports(dir)
	sort.Sort(sort.Reverse(sort.StringSlice(reports)))
	var total int64
	for _, report := range reports {
		at, ok := stallwatch.ReportTime(report)
		if !ok || at.Before(from) || at.After(to) {
			continue
		}
		// Lstat: a link in the directory is not a report.
		info, err := os.Lstat(report)
		if err != nil || !info.Mode().IsRegular() || info.Size() > memberBounds.small {
			continue
		}
		if len(kept) == memberBounds.stalls || total+info.Size() > memberBounds.stallBytes {
			truncated = true
			break
		}
		total += info.Size()
		kept = append(kept, memberFile{name: memberStallPrefix + filepath.Base(report), path: report, bytes: info.Size()})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].name < kept[j].name })
	return kept, truncated
}

// readMemberOwnPart reads this member's part for the window. The caller
// closes it.
func readMemberOwnPart(ctx context.Context, src MemberSource, from, to, now time.Time) (*memberPart, error) {
	p := newMemberPart(ctx, src, from, to, now)
	sink := &recordSink{}
	// The records are read from the whole window, also from the lines the
	// log's bound leaves out.
	var err error
	if p.logBytes, p.logSkip, err = measureNewest(ctx, p.logs, sink, from, to, memberBounds.log); err != nil {
		p.close()
		return nil, err
	}
	p.setRecords(sink, p.logSkip > 0)
	var cut bool
	if p.stalls, cut = memberStalls(src.StallDir(), from, to); cut {
		p.facts.Truncated = append(p.facts.Truncated, strings.TrimSuffix(memberStallPrefix, "/"))
	}
	p.status = src.Status(ctx)
	if err := ctx.Err(); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

func (p *memberPart) setRecords(sink *recordSink, logTruncated bool) {
	p.roles, p.leaders = sink.roles.data, sink.leaders.data
	for _, f := range []struct {
		name string
		cut  bool
	}{{MemberLogFile, logTruncated}, {MemberRoleEventsFile, sink.roles.truncated}, {MemberLeaderChangesFile, sink.leaders.truncated}} {
		if f.cut {
			p.facts.Truncated = append(p.facts.Truncated, f.name)
		}
	}
}

// partFile is one of a part's files held in memory.
type partFile struct {
	name string
	data []byte
}

// small are the part's files held in memory, member.json first.
func (p *memberPart) small() []partFile {
	// MemberFacts is strings, numbers and times: it always marshals.
	facts, _ := json.MarshalIndent(p.facts, "", "  ")
	out := []partFile{{MemberFactsFile, facts}, {MemberRoleEventsFile, p.roles}, {MemberLeaderChangesFile, p.leaders}}
	if p.status != "" {
		out = append(out, partFile{MemberStatusFile, []byte(p.status)})
	}
	return out
}

// stageMemberPart writes the collecting member's own part into dir. Its log
// and stall reports are in the collection already (under server/): only the
// small files are written here. The records are read from the log's whole
// window, not from the staged log, which may have been cut at its bound.
func stageMemberPart(ctx context.Context, dir string, src MemberSource, from, to, now time.Time) ([]memberFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := newMemberPart(ctx, src, from, to, now)
	defer p.close()
	sink := &recordSink{}
	if _, _, err := filterFiles(ctx, p.logs, sink, from, to, math.MaxInt64); err != nil {
		return nil, err
	}
	p.setRecords(sink, false)
	p.status = src.Status(ctx)
	var files []memberFile
	for _, f := range p.small() {
		local := filepath.Join(dir, f.name)
		if err := os.WriteFile(local, f.data, 0o600); err != nil {
			return nil, err
		}
		files = append(files, memberFile{name: f.name, path: local, bytes: int64(len(f.data))})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}
