package diagnostics

import (
	"archive/tar"
	"bufio"
	"bytes"
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
	// MemberPartBytes bounds what is read of another member's part.
	MemberPartBytes = MemberLogBytes + 64<<20
	// MemberPartTimeout bounds one member's answer.
	MemberPartTimeout = 2 * time.Minute
	// memberStallReports bounds the stall reports of one part, newest kept.
	memberStallReports = 64
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

// memberFile is one staged file of a part.
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

// stageMemberPart writes a member's part into dir. With log set, the
// member's log in the window is already there (the collecting member's own
// log, staged as server/gpuworks-server.log): only the small files are
// written, read from it. Otherwise the log and the stall reports in the
// window are staged too.
func stageMemberPart(ctx context.Context, dir string, src MemberSource, from, to time.Time, now time.Time, log string) ([]memberFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var files []memberFile
	facts := src.Facts()
	facts.CollectedAt, facts.From, facts.To, facts.Truncated = now, from, to, []string{}
	if log == "" {
		log = filepath.Join(dir, MemberLogFile)
		f, err := os.Create(log)
		if err != nil {
			return nil, err
		}
		n, truncated, err := logWindow(src.LogFiles(), f, from, to, MemberLogBytes)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		files = append(files, memberFile{name: MemberLogFile, path: log, bytes: n, truncated: truncated})
		reports := stallwatch.Reports(src.StallDir())
		sort.Strings(reports)
		var kept []string
		for _, report := range reports {
			if at, ok := stallwatch.ReportTime(report); ok && !at.Before(from) && !at.After(to) {
				kept = append(kept, report)
			}
		}
		if len(kept) > memberStallReports {
			kept = kept[len(kept)-memberStallReports:]
			facts.Truncated = append(facts.Truncated, strings.TrimSuffix(memberStallPrefix, "/"))
		}
		for _, report := range kept {
			if info, err := os.Stat(report); err == nil && info.Mode().IsRegular() && info.Size() <= memberSmallFile {
				files = append(files, memberFile{name: memberStallPrefix + filepath.Base(report), path: report, bytes: info.Size()})
			}
		}
	}
	for _, extract := range []struct {
		name string
		keep func([]byte) bool
	}{{MemberRoleEventsFile, roleEventLine}, {MemberLeaderChangesFile, leaderChangeLine}} {
		local := filepath.Join(dir, extract.name)
		n, err := extractLines(log, local, extract.keep)
		if err != nil {
			return nil, err
		}
		files = append(files, memberFile{name: extract.name, path: local, bytes: n})
	}
	if status := src.Status(ctx); status != "" {
		local := filepath.Join(dir, MemberStatusFile)
		if err := os.WriteFile(local, []byte(status), 0o600); err != nil {
			return nil, err
		}
		files = append(files, memberFile{name: MemberStatusFile, path: local, bytes: int64(len(status))})
	}
	for _, f := range files {
		if f.truncated {
			facts.Truncated = append(facts.Truncated, f.name)
		}
	}
	raw, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return nil, err
	}
	local := filepath.Join(dir, MemberFactsFile)
	if err := os.WriteFile(local, raw, 0o600); err != nil {
		return nil, err
	}
	files = append(files, memberFile{name: MemberFactsFile, path: local, bytes: int64(len(raw))})
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

// logWindow writes the lines of a log's generations (given newest first)
// that lie in the window, oldest first.
func logWindow(files []string, w io.Writer, from, to time.Time, max int64) (int64, bool, error) {
	var written int64
	for i := len(files) - 1; i >= 0; i-- {
		info, err := os.Stat(files[i])
		if err != nil || info.ModTime().Before(from) {
			continue // a generation last written before the window holds nothing in it
		}
		f, err := os.Open(files[i])
		if err != nil {
			continue
		}
		n, truncated, err := filterLogWindow(f, w, from, to, max-written)
		_ = f.Close()
		written += n
		if err != nil || truncated {
			return written, truncated, err
		}
	}
	return written, false, nil
}

// extractLines copies the lines of src that keep accepts to dst.
func extractLines(src, dst string, keep func([]byte) bool) (int64, error) {
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	in, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer in.Close()
	var written int64
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if line := sc.Bytes(); keep(line) {
			n, err := out.Write(append(line, '\n'))
			written += int64(n)
			if err != nil {
				return written, err
			}
		}
	}
	return written, sc.Err()
}

// WriteMemberPart writes this member's part for the window as a gzipped
// tar: what another member asks for over the member-authenticated route.
// work is a private directory the part is staged in and removed from.
func WriteMemberPart(ctx context.Context, w io.Writer, work string, src MemberSource, from, to time.Time) error {
	if !from.Before(to) || to.Sub(from) > MaxBundleWindow {
		return errors.New("the window must end after it starts and span at most 7 days")
	}
	dir, err := os.MkdirTemp(work, ".member-part-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	files, err := stageMemberPart(ctx, dir, src, from, to, time.Now(), "")
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, f := range files {
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

// readMemberPart unpacks another member's part into dir. Only the files a
// part is made of are taken, each up to its bound; anything else in the
// stream is ignored, so a member cannot place a file elsewhere.
func readMemberPart(r io.Reader, dir string) ([]memberFile, error) {
	gz, err := gzip.NewReader(io.LimitReader(r, MemberPartBytes))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	if err := os.MkdirAll(filepath.Join(dir, "stalls"), 0o700); err != nil {
		return nil, err
	}
	var files []memberFile
	seen := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, fmt.Errorf("the member's part ended early: %w", err)
		}
		name := path.Clean(h.Name)
		limit, ok := memberFileBound(name)
		if !ok || seen[name] || h.Typeflag != tar.TypeReg {
			continue
		}
		seen[name] = true
		local := filepath.Join(dir, filepath.FromSlash(name))
		f, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return files, err
		}
		n, err := io.Copy(f, io.LimitReader(tr, limit))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		// A file cut off by the stream's end is not whole either.
		files = append(files, memberFile{name: name, path: local, bytes: n, truncated: h.Size > limit || err != nil})
		if err != nil {
			return files, fmt.Errorf("the member's part ended early: %w", err)
		}
	}
	if !seen[MemberFactsFile] {
		return files, errors.New("the member's part holds no member.json")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

// memberFileBound is the size a part's file may have; ok is false for a
// name that is not a part's.
func memberFileBound(name string) (int64, bool) {
	switch name {
	case MemberLogFile:
		return MemberLogBytes, true
	case MemberFactsFile, MemberRoleEventsFile, MemberLeaderChangesFile, MemberStatusFile:
		if name == MemberRoleEventsFile {
			return MemberLogBytes, true
		}
		return memberSmallFile, true
	}
	if base, ok := strings.CutPrefix(name, memberStallPrefix); ok && base != "" && !strings.Contains(base, "/") &&
		strings.HasPrefix(base, "stall-") && strings.HasSuffix(base, ".txt") {
		return memberSmallFile, true
	}
	return 0, false
}
