package diagnostics

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// The role records as the server files them (internal/server/cluster).
const (
	roleStepDown   = `level=INFO msg="cluster database role event" observed_at=2026-11-02T11:30:00.1Z event=step_down reason=no_majority_contact contacts=1 voters=3 term=7`
	roleLoopLate   = `level=WARN msg="cluster database role event" observed_at=2026-11-02T11:29:59.9Z event=loop_late late_ms=1819 period_ms=75`
	leaderChanged  = `level=INFO msg="cluster database leader changed" old_leader=1 new_leader=2 term=8 this_member=1 leads=false reason="this member stepped down: no_majority_contact" without_leader_ms=1493`
	businessFollow = `level=INFO msg="business follows a new database leader" new_leader=2 this_member=1 leads=false learned="leader observation" agent_sessions_closed=3`
)

type fakeMember struct {
	facts    MemberFacts
	logFile  string
	stallDir string
	status   string
}

func (f fakeMember) Facts(context.Context) MemberFacts { return f.facts }
func (f fakeMember) LogFiles() []string                { return []string{f.logFile} }
func (f fakeMember) StallDir() string                  { return f.stallDir }
func (f fakeMember) Status(context.Context) string     { return f.status }

// memberFixture is a member's directories: a log with lines inside and
// outside the window, and one stall report on each side of it.
func memberFixture(t *testing.T, id uint64, address string, now time.Time) fakeMember {
	t.Helper()
	dir := t.TempDir()
	in := now.Add(-30 * time.Minute).Format(time.RFC3339Nano)
	before := now.Add(-3 * time.Hour).Format(time.RFC3339Nano)
	log := filepath.Join(dir, "server.log")
	lines := []string{
		"time=" + before + " " + roleStepDown,
		"time=" + in + " level=INFO msg=\"request served\" member=" + address,
		"time=" + in + " " + roleLoopLate,
		"time=" + in + " " + roleStepDown,
		"time=" + in + " " + leaderChanged,
		"time=" + in + " " + businessFollow,
	}
	if err := os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stalls := filepath.Join(dir, "stalls")
	_ = os.MkdirAll(stalls, 0o700)
	_ = os.WriteFile(filepath.Join(stalls, "stall-"+now.Add(-10*time.Minute).Format("20060102T150405.000Z")+".txt"), []byte("gpuworks stall report\n"), 0o600)
	_ = os.WriteFile(filepath.Join(stalls, "stall-"+now.Add(-2*time.Hour).Format("20060102T150405.000Z")+".txt"), []byte("old\n"), 0o600)
	return fakeMember{facts: MemberFacts{ID: id, Address: address, Version: "v-test", StorageTier: "slow-storage", LogFile: log},
		logFile: log, stallDir: stalls, status: "Overall: ok\nLeader: " + address + "\n"}
}

// fakeCluster answers for the other members: a part built from the
// member's fixture, or the failure the test sets for it.
type fakeCluster struct {
	t       *testing.T
	now     time.Time
	self    fakeMember
	members []ClusterMember
	parts   map[string]fakeMember
	fail    map[string]error
	// cut ends a member's part after this many bytes.
	cut map[string]int
	// membersErr is a membership that cannot be read.
	membersErr error
	// rewriteFacts changes the member.json of every part on its way.
	rewriteFacts func(*MemberFacts)
	// hang makes a member never answer within the caller's deadline.
	hang map[string]bool
	// busy makes a member answer busy this many times first.
	busy  map[string]int
	asked map[string]int
	mu    sync.Mutex
}

func (c *fakeCluster) Members(context.Context) ([]ClusterMember, error) {
	return c.members, c.membersErr
}
func (c *fakeCluster) Self() MemberSource { return c.self }

func (c *fakeCluster) OpenMemberPart(ctx context.Context, member ClusterMember, from, to time.Time) (MemberAnswer, error) {
	c.mu.Lock()
	c.asked[member.Address]++
	busy := c.busy[member.Address] > 0
	if busy {
		c.busy[member.Address]--
	}
	c.mu.Unlock()
	if busy {
		return MemberAnswer{}, errors.Join(ErrMemberBusy, errors.New("status 429"))
	}
	if c.hang[member.Address] {
		<-ctx.Done()
		return MemberAnswer{}, ctx.Err()
	}
	if err := c.fail[member.Address]; err != nil {
		return MemberAnswer{}, err
	}
	var buf bytes.Buffer
	if err := WriteMemberPart(ctx, &buf, c.parts[member.Address], from, to); err != nil {
		return MemberAnswer{}, err
	}
	raw := buf.Bytes()
	if c.rewriteFacts != nil {
		raw = rewritePartFacts(c.t, raw, c.rewriteFacts)
	}
	if n, ok := c.cut[member.Address]; ok {
		raw = raw[:n]
	}
	// Finding the member took this side 8 s before the request left (no
	// leader to ask); the offset must not include that. The member's clock
	// runs 132 s behind the collecting member's.
	sent := c.now.Add(8 * time.Second)
	return MemberAnswer{Body: io.NopCloser(bytes.NewReader(raw)), Sent: sent, Clock: sent.Add(5*time.Millisecond - 132*time.Second),
		RoundTrip: 10 * time.Millisecond, Key: "key-of-" + member.Address}, nil
}

func clusterFixture(t *testing.T, now time.Time) (*Manager, *fakeCluster) {
	t.Helper()
	m, _, _ := newManager(t)
	m.now = func() time.Time { return now }
	self := memberFixture(t, 1, "10.0.0.1:9001", now)
	m.SetSources(&fakeSources{logFile: self.logFile, dirs: []ServerDir{{Kind: "data", Path: t.TempDir()}}})
	c := &fakeCluster{t: t, self: self, now: now, busy: map[string]int{}, asked: map[string]int{},
		members: []ClusterMember{{ID: 2, Address: "10.0.0.2:9001", Role: "voter"}, {ID: 1, Address: "10.0.0.1:9001", Role: "voter", Self: true}, {ID: 3, Address: "10.0.0.3:9001", Role: "voter"}},
		parts:   map[string]fakeMember{"10.0.0.2:9001": memberFixture(t, 2, "10.0.0.2:9001", now), "10.0.0.3:9001": memberFixture(t, 3, "10.0.0.3:9001", now)},
		fail:    map[string]error{}, cut: map[string]int{}, hang: map[string]bool{}}
	m.SetCluster(c)
	return m, c
}

func collect(t *testing.T, m *Manager) (Bundle, map[string]string) {
	t.Helper()
	b, archive, err := m.CollectNow(context.Background(), BundleRequest{RequestedBy: "admin"})
	if err != nil {
		t.Fatalf("collection failed: %v", err)
	}
	if b.State != "ready" {
		t.Fatalf("state = %q (%s), want ready", b.State, b.Error)
	}
	return b, readArchive(t, archive)
}

func itemsByPath(b Bundle) map[string]BundleItem {
	out := map[string]BundleItem{}
	for _, it := range b.Items {
		out[it.Path] = it
	}
	return out
}

func TestClusterCollectionHoldsEveryMembersRecords(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m, _ := clusterFixture(t, now)
	b, files := collect(t, m)

	for _, name := range []string{"10.0.0.2_9001", "10.0.0.3_9001"} {
		log := files["members/"+name+"/server.log"]
		if strings.Count(log, "\n") != 5 || strings.Contains(log, now.Add(-3*time.Hour).Format(time.RFC3339Nano)) {
			t.Fatalf("%s log is not the window's five lines:\n%s", name, log)
		}
		if got := files["members/"+name+"/role-events.log"]; strings.Count(got, "\n") != 2 || !strings.Contains(got, "event=step_down reason=no_majority_contact") || !strings.Contains(got, "event=loop_late late_ms=1819") {
			t.Fatalf("%s role events:\n%s", name, got)
		}
		if got := files["members/"+name+"/leader-changes.log"]; strings.Count(got, "\n") != 2 || !strings.Contains(got, `msg="cluster database leader changed"`) || !strings.Contains(got, `msg="business follows a new database leader"`) {
			t.Fatalf("%s leader changes:\n%s", name, got)
		}
		stalls := 0
		for path := range files {
			if strings.HasPrefix(path, "members/"+name+"/stalls/") {
				stalls++
			}
		}
		if stalls != 1 {
			t.Fatalf("%s has %d stall reports, want the one in the window", name, stalls)
		}
		if !strings.Contains(files["members/"+name+"/cluster-status.txt"], "Overall: ok") {
			t.Fatalf("%s has no status report", name)
		}
	}
	// The collecting member: its log stays under server/, its records are
	// read out of it.
	if _, dup := files["members/10.0.0.1_9001/server.log"]; dup {
		t.Fatal("the collecting member's log is in the collection twice")
	}
	if got := files["members/10.0.0.1_9001/role-events.log"]; strings.Count(got, "\n") != 2 {
		t.Fatalf("collecting member's role events:\n%s", got)
	}
	var facts MemberFacts
	if err := json.Unmarshal([]byte(files["members/10.0.0.2_9001/member.json"]), &facts); err != nil || facts.ID != 2 || facts.StorageTier != "slow-storage" || facts.CollectedAt.IsZero() || len(facts.Truncated) != 0 {
		t.Fatalf("member.json = %+v, %v", facts, err)
	}

	const wantIndex = `{
  "complete": true,
  "members": [
    {
      "name": "10.0.0.1_9001",
      "id": "1",
      "address": "10.0.0.1:9001",
      "role": "voter",
      "self": true
    },
    {
      "name": "10.0.0.2_9001",
      "id": "2",
      "address": "10.0.0.2:9001",
      "role": "voter",
      "self": false,
      "answered_key": "key-of-10.0.0.2:9001",
      "member_clock": "2026-11-02T11:57:56.005Z",
      "clock_offset_ms": -132000,
      "round_trip_ms": 10
    },
    {
      "name": "10.0.0.3_9001",
      "id": "3",
      "address": "10.0.0.3:9001",
      "role": "voter",
      "self": false,
      "answered_key": "key-of-10.0.0.3:9001",
      "member_clock": "2026-11-02T11:57:56.005Z",
      "clock_offset_ms": -132000,
      "round_trip_ms": 10
    }
  ]
}`
	if got := files["members/index.json"]; got != wantIndex {
		t.Fatalf("members/index.json:\n%s\nwant:\n%s", got, wantIndex)
	}
	items := itemsByPath(b)
	if it := items["members/10.0.0.2_9001/server.log"]; it.Status != proto.CheckPass || it.ID() != BundleItemMemberFile {
		t.Fatalf("member log item = %+v (%s)", it, it.ID())
	}
	if it := items["members/index.json"]; it.Status != proto.CheckPass || it.ID() != BundleItemMemberIndex {
		t.Fatalf("index item = %+v (%s)", it, it.ID())
	}
}

func TestAMemberThatDoesNotAnswerIsRecordedAndTheCollectionStillStands(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	refused := errors.Join(ErrMemberRefused, errors.New("status 403"))
	for _, tc := range []struct {
		name     string
		arrange  func(*fakeCluster)
		missing  string
		evidence string
		// kept counts the files of the member that are still in the archive.
		kept bool
	}{
		{"unreachable", func(c *fakeCluster) {
			c.fail["10.0.0.3:9001"] = errors.New("dial tcp 10.0.0.3:9443: connect: connection refused")
		}, "unreachable", "connection refused", false},
		{"refused", func(c *fakeCluster) { c.fail["10.0.0.3:9001"] = refused }, "refused", "status 403", false},
		{"timeout", func(c *fakeCluster) { c.fail["10.0.0.3:9001"] = context.DeadlineExceeded }, "timeout", "deadline exceeded", false},
		{"partial stream", func(c *fakeCluster) { c.cut["10.0.0.3:9001"] = 200 }, "failed", "ended early", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c := clusterFixture(t, now)
			tc.arrange(c)
			b, files := collect(t, m)
			var index MemberIndex
			if err := json.Unmarshal([]byte(files["members/index.json"]), &index); err != nil {
				t.Fatal(err)
			}
			if index.Complete || len(index.Members) != 3 {
				t.Fatalf("index = %+v, want incomplete with three members", index)
			}
			gone := index.Members[2]
			if gone.Missing != tc.missing || !strings.Contains(gone.Evidence, tc.evidence) {
				t.Fatalf("missing member = %+v, want %s (%s)", gone, tc.missing, tc.evidence)
			}
			if index.Members[0].Missing != "" || index.Members[1].Missing != "" {
				t.Fatalf("the other members were not collected: %+v", index.Members)
			}
			it := itemsByPath(b)["members/10.0.0.3_9001"]
			if it.Status != proto.CheckFail || it.Code != messages.CheckDiagnosticsMemberUnavailable || it.ID() != BundleItemMember ||
				!strings.HasPrefix(it.Evidence, tc.missing+": ") || !strings.Contains(it.Message(), "does not show what that member saw") {
				t.Fatalf("item = %+v (%s)", it, it.ID())
			}
			if _, ok := files["members/10.0.0.2_9001/server.log"]; !ok {
				t.Fatal("the member that answered is not in the collection")
			}
			if _, ok := files["members/10.0.0.3_9001/server.log"]; ok != tc.kept {
				t.Fatalf("missing member's log in the archive = %v, want %v", ok, tc.kept)
			}
		})
	}
}

func TestAMemberThatNeverAnswersIsGivenUpOnAtItsDeadline(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m, c := clusterFixture(t, now)
	c.hang["10.0.0.3:9001"] = true
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	b, archive, err := m.CollectNow(ctx, BundleRequest{RequestedBy: "admin"})
	if err != nil || b.State != "ready" {
		t.Fatalf("collection = %q, %v; a silent member must not fail it", b.State, err)
	}
	var index MemberIndex
	_ = json.Unmarshal([]byte(readArchive(t, archive)["members/index.json"]), &index)
	if index.Complete || index.Members[2].Missing != "timeout" {
		t.Fatalf("index = %+v, want the silent member missing by timeout", index)
	}
}

func TestASingleServersCollectionHasNoMembers(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m, c := clusterFixture(t, now)
	c.members = nil // a data directory that is not a cluster member's
	b, files := collect(t, m)
	for path := range files {
		if strings.HasPrefix(path, "members/") {
			t.Fatalf("a single server's collection holds %s", path)
		}
	}
	if missingMembers(b) != 0 {
		t.Fatalf("a single server's collection misses members: %+v", b.Items)
	}
}

// A member's answer is another machine's bytes: only a part's own files are
// taken, each inside the member's directory.
func TestAMembersPartCannotPlaceFilesOutsideItsDirectory(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(h tar.Header, body string) {
		h.Size, h.Mode = int64(len(body)), 0o600
		if h.Typeflag == tar.TypeSymlink {
			h.Size = 0
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			_, _ = tw.Write([]byte(body))
		}
	}
	write(tar.Header{Name: "member.json"}, `{"id":2,"address":"10.0.0.2:9001","server_version":"v","collected_at":"2026-11-02T12:00:00Z","from":"2026-11-02T11:00:00Z","to":"2026-11-02T12:00:00Z","truncated":[]}`)
	write(tar.Header{Name: "../escaped.txt"}, "x")
	write(tar.Header{Name: "/etc/passwd"}, "x")
	write(tar.Header{Name: "stalls/../../escaped2.txt"}, "x")
	write(tar.Header{Name: "stalls/stall-20261102T115000.000Z.txt"}, "report")
	write(tar.Header{Name: "stalls/notes.txt"}, "x")
	write(tar.Header{Name: "master.key"}, "secret")
	write(tar.Header{Name: "server.log", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"}, "")
	write(tar.Header{Name: "role-events.log"}, "one\n")
	write(tar.Header{Name: "role-events.log"}, "a second file of the same name\n")
	_ = tw.Close()
	_ = gz.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "members", "m2")
	var bundle atomic.Int64
	bundle.Store(memberBounds.all)
	files, err := readMemberPart(&buf, dir, &partBudget{member: memberBounds.part, bundle: &bundle})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.name)
	}
	if got := strings.Join(names, ","); got != "member.json,role-events.log,stalls/stall-20261102T115000.000Z.txt" {
		t.Fatalf("taken = %s", got)
	}
	var found []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if got := strings.Join(found, ","); got != "members/m2/member.json,members/m2/role-events.log,members/m2/stalls/stall-20261102T115000.000Z.txt" {
		t.Fatalf("files on disk = %s", got)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "role-events.log")); string(raw) != "one\n" {
		t.Fatalf("role-events.log = %q, want the first one", raw)
	}
}

// A part is made of named files, never of a directory's contents: nothing
// beside the log is read. This fails the day a part starts sweeping a
// directory. What a log line itself may carry is the log sources' own
// matter (the request record's redaction).
func TestAMembersPartIsNamedFilesNotADirectorySweep(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	member := memberFixture(t, 2, "10.0.0.2:9001", now)
	data := filepath.Dir(member.logFile)
	secrets := map[string]string{
		"master.key":                        "MASTERKEY-5f2b7c",
		"cluster-enrollment/server.key":     "-----BEGIN PRIVATE KEY-----MEMBERKEY-91aa",
		"cluster-enrollment/ca.key":         "-----BEGIN PRIVATE KEY-----CAKEY-77d0",
		"cluster-enrollment/config.json":    `{"token":"JOINTOKEN-3c1e"}`,
		"stalls/admin-password":             "ADMINPASS-0b6d",
		"stalls/license-private.pem":        "LICENSEKEY-e42f",
		"gpuworks.db.dqlite/.transport/key": "TRANSPORTKEY-88c3",
	}
	for name, body := range secrets {
		path := filepath.Join(data, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := WriteMemberPart(context.Background(), &buf, member, now.Add(-time.Hour), now); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	var all bytes.Buffer
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		_, _ = io.Copy(&all, tr)
	}
	if got := strings.Join(names, ","); !strings.HasPrefix(got, "member.json,role-events.log,leader-changes.log,cluster-status.txt,server.log,stalls/stall-") || len(names) != 6 {
		t.Fatalf("part holds %s", got)
	}
	for name, body := range secrets {
		if bytes.Contains(all.Bytes(), []byte(body)) {
			t.Fatalf("the part carries the content of %s", name)
		}
	}
}

func TestAMembersPartRefusesAWindowItCannotServe(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	member := memberFixture(t, 2, "10.0.0.2:9001", now)
	for _, w := range [][2]time.Time{{now, now.Add(-time.Hour)}, {now.Add(-8 * 24 * time.Hour), now}} {
		if err := WriteMemberPart(context.Background(), io.Discard, member, w[0], w[1]); err == nil || !strings.Contains(err.Error(), "span at most 7 days") {
			t.Fatalf("window %v..%v: %v, want a refusal naming the bound", w[0], w[1], err)
		}
	}
}

// A whole cluster collection, likewise, holds named files only.
func TestAClusterCollectionIsNamedFilesNotADirectorySweep(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m, c := clusterFixture(t, now)
	secrets := map[string]string{
		"master.key":                             "MASTERKEY-5f2b7c",
		"cluster-enrollment/server.key":          "-----BEGIN PRIVATE KEY-----MEMBERKEY-91aa",
		"cluster-enrollment/ca.key":              "-----BEGIN PRIVATE KEY-----CAKEY-77d0",
		"cluster-enrollment/config.json":         `{"token":"JOINTOKEN-3c1e"}`,
		"gpuworks.db.join/key.pem":               "-----BEGIN PRIVATE KEY-----JOINKEY-12ab",
		"gpuworks.db.dqlite/.transport/key":      "TRANSPORTKEY-88c3",
		"stalls/license-private.pem":             "LICENSEKEY-e42f",
		"stalls/stall-20261102T115500.000Z.json": "ADMINPASS-0b6d",
	}
	for _, member := range []fakeMember{c.self, c.parts["10.0.0.2:9001"], c.parts["10.0.0.3:9001"]} {
		for name, body := range secrets {
			path := filepath.Join(filepath.Dir(member.logFile), filepath.FromSlash(name))
			_ = os.MkdirAll(filepath.Dir(path), 0o700)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, files := collect(t, m)
	if len(files) < 15 {
		t.Fatalf("the collection holds only %d files", len(files))
	}
	for path, body := range files {
		for name, secret := range secrets {
			if strings.Contains(body, secret) || strings.Contains(body, "PRIVATE KEY") {
				t.Fatalf("%s carries the content of %s", path, name)
			}
		}
	}
}

// rewritePartFacts repacks a part with its member.json changed.
func rewritePartFacts(t *testing.T, raw []byte, change func(*MemberFacts)) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if h.Name == MemberFactsFile {
			var facts MemberFacts
			if err := json.Unmarshal(body, &facts); err != nil {
				t.Fatal(err)
			}
			change(&facts)
			body, _ = json.Marshal(facts)
		}
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	_ = zw.Close()
	return out.Bytes()
}
