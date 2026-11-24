package diagnostics

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func partFiles(t *testing.T, raw []byte) (names []string, files map[string]string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	files = map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, files
		}
		if err != nil {
			t.Fatalf("the part is not a whole archive: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("%s: %v", h.Name, err)
		}
		names = append(names, h.Name)
		files[h.Name] = string(body)
	}
}

// Why a reached member gave no part is told apart: a member that is busy is
// asked once more, a member not yet upgraded is not "unreachable".
func TestWhyAMemberGaveNoPartIsToldApart(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	previous := memberBusyWait
	memberBusyWait = 10 * time.Millisecond
	t.Cleanup(func() { memberBusyWait = previous })
	const third = "10.0.0.3:9001"
	for _, tc := range []struct {
		name      string
		arrange   func(*fakeCluster)
		collected bool
		missing   string
		asked     int
	}{
		{"busy once, then answers", func(c *fakeCluster) { c.busy[third] = 1 }, true, "", 2},
		{"busy twice", func(c *fakeCluster) { c.busy[third] = 2 }, false, "busy", 2},
		{"a release without the route", func(c *fakeCluster) { c.fail[third] = errors.Join(ErrMemberUnsupported, errors.New("status 404")) }, false, "unsupported", 1},
		{"answers with a failure", func(c *fakeCluster) { c.fail[third] = errors.Join(ErrMemberFailed, errors.New("status 503")) }, false, "failed", 1},
		{"answers as another member", func(c *fakeCluster) { c.parts[third] = c.parts["10.0.0.2:9001"] }, false, "failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c := clusterFixture(t, now)
			tc.arrange(c)
			b, files := collect(t, m)
			var index MemberIndex
			if err := json.Unmarshal([]byte(files["members/index.json"]), &index); err != nil {
				t.Fatal(err)
			}
			got := index.Members[2]
			if got.Collected != tc.collected || got.Missing != tc.missing || index.Complete != tc.collected {
				t.Fatalf("member = %+v, complete = %v", got, index.Complete)
			}
			if c.asked[third] != tc.asked {
				t.Fatalf("asked %d times, want %d", c.asked[third], tc.asked)
			}
			// The manifest says so too.
			var manifest Bundle
			if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil || manifest.Complete == nil || *manifest.Complete != tc.collected {
				t.Fatalf("manifest complete = %v (%v), want %v", manifest.Complete, err, tc.collected)
			}
			if b.Complete == nil || *b.Complete != tc.collected {
				t.Fatalf("bundle complete = %v", b.Complete)
			}
			if tc.name == "answers as another member" && !strings.Contains(got.Evidence, "asked member 3 at 10.0.0.3:9001, the part is member 2's at 10.0.0.2:9001") {
				t.Fatalf("evidence = %q", got.Evidence)
			}
		})
	}
}

func TestASingleServersManifestSaysNothingOfCompleteness(t *testing.T) {
	m, c := clusterFixture(t, time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	c.members = nil
	_, files := collect(t, m)
	if strings.Contains(files["manifest.json"], `"complete"`) {
		t.Fatalf("manifest = %s", files["manifest.json"])
	}
}

// growingMember's log grows between the pass that measures the window and
// the pass that writes it (Status runs in between): lines are appended and
// the last line, unfinished when measured, is completed.
type growingMember struct {
	fakeMember
	grow string
}

func (g growingMember) Status(context.Context) string {
	f, _ := os.OpenFile(g.logFile, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(g.grow)
	_ = f.Close()
	return "Overall: ok\n"
}

func TestAPartIsAWholeArchiveWhileTheLogGrows(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	in := now.Add(-30 * time.Minute).Format(time.RFC3339Nano)
	for _, tc := range []struct{ name, start, grow, want string }{
		{"whole lines appended", "time=" + in + " one\n", "time=" + in + " two\n", "time=" + in + " one\n"},
		{"the last line was unfinished", "time=" + in + " one\ntime=" + in + " tw", "o and more\n", "time=" + in + " one\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member := memberFixture(t, 2, "10.0.0.2:9001", now)
			if err := os.WriteFile(member.logFile, []byte(tc.start), 0o600); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err := WriteMemberPart(context.Background(), &buf, growingMember{member, tc.grow}, now.Add(-time.Hour), now); err != nil {
				t.Fatal(err)
			}
			_, files := partFiles(t, buf.Bytes())
			if got := files[MemberLogFile]; len(got) != len(tc.start)+strings.Count(tc.start[len(tc.start)-1:], "w") || !strings.HasPrefix(got, tc.want) || strings.Contains(got, "more") {
				t.Fatalf("log = %q", got)
			}
		})
	}
}

// A part is read and written without staging anything on the member's disk,
// and it stops when its context does.
func TestAPartStopsWhenItsRequestDoesAndWritesNothingToDisk(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	member := memberFixture(t, 2, "10.0.0.2:9001", now)
	line := "time=" + now.Add(-30*time.Minute).Format(time.RFC3339Nano) + " " + strings.Repeat("x", 200) + "\n"
	if err := os.WriteFile(member.logFile, []byte(strings.Repeat(line, 20000)), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(member.logFile)
	before := treeNames(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WriteMemberPart(ctx, io.Discard, member, now.Add(-time.Hour), now); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	// Cancelled midway: the writer cancels at the archive's first bytes,
	// before the log is written.
	ctx, cancel = context.WithCancel(context.Background())
	w := &cancellingWriter{cancel: cancel}
	if err := WriteMemberPart(ctx, w, member, now.Add(-time.Hour), now); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	var buf bytes.Buffer
	if err := WriteMemberPart(context.Background(), &buf, member, now.Add(-time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if after := treeNames(t, dir); after != before {
		t.Fatalf("the member's directory changed:\n%s\nwas\n%s", after, before)
	}
}

type cancellingWriter struct {
	n, after int64
	cancel   func()
}

func (c *cancellingWriter) Write(p []byte) (int, error) {
	if c.n += int64(len(p)); c.n > c.after {
		c.cancel()
	}
	return len(p), nil
}

func treeNames(t *testing.T, root string) string {
	t.Helper()
	var names []string
	_ = filepath.WalkDir(root, func(p string, _ os.DirEntry, _ error) error {
		names = append(names, p)
		return nil
	})
	return strings.Join(names, "\n")
}

// hostilePart is a member's answer with many stall reports of a given size.
func hostilePart(t *testing.T, stalls int, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(body)
	}
	write("member.json", []byte(`{"id":2,"address":"10.0.0.2:9001","server_version":"v","collected_at":"2026-11-02T12:00:00Z","from":"2026-11-02T11:00:00Z","to":"2026-11-02T12:00:00Z","truncated":[]}`))
	body := bytes.Repeat([]byte("s"), size)
	for i := 0; i < stalls; i++ {
		write(fmt.Sprintf("stalls/stall-20261102T11%04d.000Z.txt", i), body)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// A member cannot fill the collecting member's disk: stall reports are
// taken up to their count, a part up to its budget, all parts together up
// to the bundle's.
func TestAMembersPartIsUnpackedWithinItsBounds(t *testing.T) {
	budget := func(member, bundle int64) *partBudget {
		var b atomic.Int64
		b.Store(bundle)
		return &partBudget{member: member, bundle: &b}
	}
	t.Run("stall reports beyond the count are left", func(t *testing.T) {
		files, err := readMemberPart(bytes.NewReader(hostilePart(t, 500, 10)), t.TempDir(), budget(MemberPartBytes, MembersBytes))
		if err != nil || len(files) != 1+memberStallReports {
			t.Fatalf("%d files, %v; want member.json and %d stall reports", len(files), err, memberStallReports)
		}
	})
	for _, tc := range []struct {
		name           string
		member, bundle int64
	}{{"the member's budget", 1 << 20, MembersBytes}, {"the bundle's budget", MemberPartBytes, 1 << 20}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			b := budget(tc.member, tc.bundle)
			files, err := readMemberPart(bytes.NewReader(hostilePart(t, 40, 100<<10)), dir, b)
			// What arrived is kept, but the part is not whole and says so.
			if !errors.Is(err, errPartBudget) {
				t.Fatalf("err = %v, want the budget's", err)
			}
			var total int64
			for _, f := range files {
				total += f.bytes
			}
			last := files[len(files)-1]
			if total > 1<<20 || total < 1<<20-100<<10 || !last.truncated {
				t.Fatalf("unpacked %d bytes, last file %+v; want the budget of 1 MiB and the last file cut", total, last)
			}
			var onDisk int64
			_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, _ error) error {
				if info, err := d.Info(); err == nil && !d.IsDir() {
					onDisk += info.Size()
				}
				return nil
			})
			if onDisk != total || b.bundle.Load() < 0 {
				t.Fatalf("on disk %d, counted %d, bundle budget left %d", onDisk, total, b.bundle.Load())
			}
		})
	}
	t.Run("two members share the bundle's budget", func(t *testing.T) {
		var shared atomic.Int64
		shared.Store(1 << 20)
		var total int64
		for i := 0; i < 2; i++ {
			files, err := readMemberPart(bytes.NewReader(hostilePart(t, 40, 100<<10)), t.TempDir(), &partBudget{member: MemberPartBytes, bundle: &shared})
			if !errors.Is(err, errPartBudget) {
				t.Fatalf("part %d: err = %v, want the budget's", i, err)
			}
			for _, f := range files {
				total += f.bytes
			}
		}
		if total > 1<<20 {
			t.Fatalf("two parts unpacked %d bytes of a 1 MiB bundle budget", total)
		}
	})
}
