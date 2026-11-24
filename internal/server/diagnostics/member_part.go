package diagnostics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

// Bounds of one member's part.
const (
	// MemberLogBytes bounds a member's log in a part.
	MemberLogBytes = int64(64 << 20)
	// MemberPartBytes bounds one member's part as it is unpacked, and
	// MembersBytes every member's together.
	MemberPartBytes = MemberLogBytes + 64<<20
	MembersBytes    = int64(512 << 20)
	// MemberPartTimeout bounds one member's answer.
	MemberPartTimeout = 2 * time.Minute
	// memberStallReports bounds the stall reports of one part, newest kept.
	// and memberStallBytes their size together.
	memberStallReports = 64
	memberStallBytes   = int64(32 << 20)
	memberSmallFile    = int64(8 << 20)
)

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
	Facts() MemberFacts
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

// capped keeps what fits its bound of the lines it is given.
type capped struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *capped) add(line []byte) {
	if int64(c.buf.Len()+len(line)) > memberSmallFile {
		c.truncated = true
		return
	}
	c.buf.Write(line)
}

// recordSink is given a log's lines one Write at a time (filterLogWindow
// does): it counts them, keeps the role records apart, and passes the lines
// on when out is set.
type recordSink struct {
	out            io.Writer
	n              int64
	roles, leaders capped
}

func (s *recordSink) Write(line []byte) (int, error) {
	switch {
	case roleEventLine(line):
		s.roles.add(line)
	case leaderChangeLine(line):
		s.leaders.add(line)
	}
	s.n += int64(len(line))
	if s.out != nil {
		return s.out.Write(line)
	}
	return len(line), nil
}

// memberPart is a member's part, read and measured, before it is written.
// Nothing is staged on disk: the log's generations stay open between the
// pass that measures the window and the pass that writes it, so a rotation
// in between changes nothing, and the small files are held in memory.
type memberPart struct {
	facts          MemberFacts
	from, to       time.Time
	logs           []*os.File // oldest first
	logBytes       int64
	roles, leaders []byte
	status         string
	stalls         []memberFile
}

func (p *memberPart) close() {
	for _, f := range p.logs {
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

// logWindow writes the lines of a log's generations (given newest first)
// that lie in the window, oldest first.
func logWindow(files []string, w io.Writer, from, to time.Time, max int64) (int64, bool, error) {
	opened := openLogWindow(files, from)
	defer func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}()
	return filterFiles(context.Background(), opened, w, from, to, max)
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
		info, err := os.Stat(report)
		if err != nil || !info.Mode().IsRegular() || info.Size() > memberSmallFile {
			continue
		}
		if len(kept) == memberStallReports || total+info.Size() > memberStallBytes {
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
	p := &memberPart{facts: src.Facts(), from: from, to: to}
	p.facts.CollectedAt, p.facts.From, p.facts.To, p.facts.Truncated = now, from, to, []string{}
	p.logs = openLogWindow(src.LogFiles(), from)
	sink := &recordSink{}
	n, truncated, err := filterFiles(ctx, p.logs, sink, from, to, MemberLogBytes)
	if err != nil {
		p.close()
		return nil, err
	}
	p.logBytes = n
	p.setRecords(sink, truncated)
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
	p.roles, p.leaders = sink.roles.buf.Bytes(), sink.leaders.buf.Bytes()
	for _, f := range []struct {
		name string
		cut  bool
	}{{MemberLogFile, logTruncated}, {MemberRoleEventsFile, sink.roles.truncated}, {MemberLeaderChangesFile, sink.leaders.truncated}} {
		if f.cut {
			p.facts.Truncated = append(p.facts.Truncated, f.name)
		}
	}
}

// small are the part's files held in memory, member.json first.
func (p *memberPart) small() ([]struct {
	name string
	data []byte
}, error) {
	facts, err := json.MarshalIndent(p.facts, "", "  ")
	if err != nil {
		return nil, err
	}
	out := []struct {
		name string
		data []byte
	}{{MemberFactsFile, facts}, {MemberRoleEventsFile, p.roles}, {MemberLeaderChangesFile, p.leaders}}
	if p.status != "" {
		out = append(out, struct {
			name string
			data []byte
		}{MemberStatusFile, []byte(p.status)})
	}
	return out, nil
}

// stageMemberPart writes the collecting member's own part into dir. Its log
// in the window is already staged (as server/gpuworks-server.log, at log)
// and so are its stall reports: only the small files are written here, the
// records read out of that log.
func stageMemberPart(ctx context.Context, dir string, src MemberSource, from, to, now time.Time, log string) ([]memberFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := &memberPart{facts: src.Facts(), from: from, to: to}
	p.facts.CollectedAt, p.facts.From, p.facts.To, p.facts.Truncated = now, from, to, []string{}
	sink := &recordSink{}
	if f, err := os.Open(log); err == nil {
		sc := bufio.NewScanner(ctxReader{ctx, f})
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			_, _ = sink.Write(append(sc.Bytes(), '\n'))
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	p.setRecords(sink, false)
	p.status = src.Status(ctx)
	small, err := p.small()
	if err != nil {
		return nil, err
	}
	var files []memberFile
	for _, f := range small {
		local := filepath.Join(dir, f.name)
		if err := os.WriteFile(local, f.data, 0o600); err != nil {
			return nil, err
		}
		files = append(files, memberFile{name: f.name, path: local, bytes: int64(len(f.data))})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}
