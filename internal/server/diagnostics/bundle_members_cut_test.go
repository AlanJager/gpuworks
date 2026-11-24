package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func lowerBound[T any](t *testing.T, bound *T, value T) {
	t.Helper()
	previous := *bound
	*bound = value
	t.Cleanup(func() { *bound = previous })
}

// busyLog is a member's log whose window does not fit a small bound: an
// early role record, filler, and the leader change that came last.
func busyLog(t *testing.T, member fakeMember, now time.Time) (first, last string) {
	t.Helper()
	at := func(minutes int) string {
		return "time=" + now.Add(-time.Duration(minutes)*time.Minute).Format(time.RFC3339Nano)
	}
	first = at(50) + " " + roleStepDown
	last = at(1) + " " + leaderChanged
	lines := []string{first}
	for i := 0; i < 200; i++ {
		lines = append(lines, fmt.Sprintf("%s level=INFO msg=\"request served\" n=%03d", at(40), i))
	}
	lines = append(lines, last)
	if err := os.WriteFile(member.logFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return first, last
}

// A log that does not fit its bound keeps its newest lines, and the role
// records are read from the whole window, also from the lines left out.
func TestAPartCutAtItsBoundKeepsTheNewestLinesAndEveryRoleRecord(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	lowerBound(t, &memberLogBound, int64(2000))
	member := memberFixture(t, 2, "10.0.0.2:9001", now)
	first, last := busyLog(t, member, now)
	var buf bytes.Buffer
	if err := WriteMemberPart(context.Background(), &buf, member, now.Add(-time.Hour), now); err != nil {
		t.Fatal(err)
	}
	_, files := partFiles(t, buf.Bytes())
	log := files[MemberLogFile]
	if len(log) > 2000 || len(log) < 1900 || !strings.HasSuffix(log, last+"\n") || strings.Contains(log, "n=000") || !strings.Contains(log, "n=199") {
		t.Fatalf("the cut log (%d bytes) is not the window's newest lines:\n%s", len(log), log)
	}
	if !strings.HasPrefix(log, "time=") {
		t.Fatalf("the cut log starts inside a line:\n%.80s", log)
	}
	if got := files[MemberRoleEventsFile]; got != first+"\n" {
		t.Fatalf("the role record in the lines left out is missing:\n%s", got)
	}
	if got := files[MemberLeaderChangesFile]; got != last+"\n" {
		t.Fatalf("leader changes:\n%s", got)
	}
	var facts MemberFacts
	if err := json.Unmarshal([]byte(files[MemberFactsFile]), &facts); err != nil || strings.Join(facts.Truncated, ",") != MemberLogFile {
		t.Fatalf("member.json truncated = %v (%v), want the log alone", facts.Truncated, err)
	}
}

// The server's own log is cut the same way, across its generations.
func TestTheServerLogCutAtItsBoundKeepsTheNewestLines(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	line := func(generation string, i int) string {
		return fmt.Sprintf("time=%s level=INFO msg=%s-%02d", now.Add(-10*time.Minute).Format(time.RFC3339Nano), generation, i)
	}
	var older, newer []string
	for i := 0; i < 20; i++ {
		older, newer = append(older, line("older", i)), append(newer, line("newer", i))
	}
	current, rotated := filepath.Join(dir, "server.log"), filepath.Join(dir, "server.log.1")
	_ = os.WriteFile(rotated, []byte(strings.Join(older, "\n")+"\n"), 0o600)
	_ = os.WriteFile(current, []byte(strings.Join(newer, "\n")+"\n"), 0o600)
	whole := strings.Join(append(older, newer...), "\n") + "\n"
	lineLen := len(older[0]) + 1

	var out bytes.Buffer
	n, truncated, err := logWindow(context.Background(), []string{current, rotated}, &out, now.Add(-time.Hour), now, int64(25*lineLen+7))
	if err != nil || !truncated || n != int64(out.Len()) {
		t.Fatalf("n=%d truncated=%v err=%v", n, truncated, err)
	}
	if want := whole[15*lineLen:]; out.String() != want {
		t.Fatalf("kept:\n%s\nwant the newest 25 lines, oldest first:\n%s", out.String(), want)
	}
	out.Reset()
	if n, truncated, err := logWindow(context.Background(), []string{current, rotated}, &out, now.Add(-time.Hour), now, int64(len(whole))); err != nil || truncated || out.String() != whole || n != int64(len(whole)) {
		t.Fatalf("a log that fits: n=%d truncated=%v err=%v", n, truncated, err)
	}
}

// Role records beyond their own bound keep the newest and say so.
func TestRoleRecordsBeyondTheirBoundKeepTheNewestAndAreFlagged(t *testing.T) {
	var c capped
	line := func(i int) []byte {
		return []byte(fmt.Sprintf("%s n=%07d %s\n", roleLoopLate, i, strings.Repeat("x", 100)))
	}
	count := int(memberSmallFile)/len(line(0)) + 500
	for i := 0; i < count; i++ {
		c.add(line(i))
	}
	if !c.truncated || int64(len(c.data)) > memberSmallFile || !bytes.HasSuffix(c.data, line(count-1)) || bytes.Contains(c.data, line(0)) || !bytes.HasPrefix(c.data, []byte("level=")) {
		t.Fatalf("truncated=%v, %d bytes", c.truncated, len(c.data))
	}
	sink := &recordSink{roles: c}
	p := &memberPart{}
	p.setRecords(sink, false)
	if strings.Join(p.facts.Truncated, ",") != MemberRoleEventsFile {
		t.Fatalf("member.json truncated = %v", p.facts.Truncated)
	}
}

// What a member cut itself is in the index and the manifest, not only in
// its member.json: a cut log is not a pass.
func TestWhatAMemberCutItselfShowsInTheIndexAndTheManifest(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	lowerBound(t, &memberLogBound, int64(2000))
	m, c := clusterFixture(t, now)
	third := c.parts["10.0.0.3:9001"]
	busyLog(t, third, now)
	for i := 0; i < memberStallReports+5; i++ {
		name := "stall-" + now.Add(-time.Duration(i+1)*time.Second).Format("20060102T150405.000Z") + ".txt"
		_ = os.WriteFile(filepath.Join(third.stallDir, name), []byte("gpuworks stall report\n"), 0o600)
	}
	b, files := collect(t, m)
	var index MemberIndex
	if err := json.Unmarshal([]byte(files["members/index.json"]), &index); err != nil {
		t.Fatal(err)
	}
	got := index.Members[2]
	if !got.Collected || strings.Join(got.Truncated, ",") != "server.log,stalls" {
		t.Fatalf("member = %+v, want collected with its log and stall reports cut", got)
	}
	if other := index.Members[1]; len(other.Truncated) != 0 {
		t.Fatalf("a member that cut nothing = %+v", other)
	}
	items := itemsByPath(b)
	for _, path := range []string{"members/10.0.0.3_9001/server.log", "members/10.0.0.3_9001/stalls"} {
		if it := items[path]; it.Status != proto.CheckWarn || it.Code != messages.CheckDiagnosticsItemTruncated {
			t.Fatalf("%s = %+v, want a warning that it was cut", path, it)
		}
	}
	if it := items["members/10.0.0.2_9001/server.log"]; it.Status != proto.CheckPass {
		t.Fatalf("a whole log = %+v", it)
	}
	var manifest Bundle
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil || itemsByPath(manifest)["members/10.0.0.3_9001/server.log"].Status != proto.CheckWarn {
		t.Fatalf("the manifest does not show the cut log (%v)", err)
	}
	stalls := 0
	for path := range files {
		if strings.HasPrefix(path, "members/10.0.0.3_9001/stalls/") {
			stalls++
		}
	}
	if stalls != memberStallReports {
		t.Fatalf("%d stall reports, want the newest %d", stalls, memberStallReports)
	}
}

// Running out of the collection's budget in the middle of a part: what
// arrived is kept, the member is not recorded as collected, and the
// collection does not call itself complete.
func TestAPartCutByTheCollectionsBudgetIsNotRecordedAsCollected(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	lowerBound(t, &membersBudget, int64(2500))
	m, _ := clusterFixture(t, now)
	b, files := collect(t, m)
	var index MemberIndex
	if err := json.Unmarshal([]byte(files["members/index.json"]), &index); err != nil {
		t.Fatal(err)
	}
	cut := 0
	for _, member := range index.Members {
		if member.Self {
			continue
		}
		if !member.Collected {
			cut++
			if member.Missing != "failed" || !strings.Contains(member.Evidence, "size bound of the members' parts") {
				t.Fatalf("member = %+v", member)
			}
			if it := itemsByPath(b)["members/"+member.Name]; it.Status != proto.CheckFail || it.Code != messages.CheckDiagnosticsMemberUnavailable {
				t.Fatalf("item = %+v", it)
			}
		}
	}
	if cut == 0 || index.Complete || b.Complete == nil || *b.Complete {
		t.Fatalf("%d members cut, index complete %v, manifest complete %v", cut, index.Complete, b.Complete)
	}
}

// Two addresses that read alike as a directory name get a directory each.
func TestMembersWhoseAddressesReadAlikeGetADirectoryEach(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m, c := clusterFixture(t, now)
	c.members[2].Address = "10.0.0.2_9001"
	twin := memberFixture(t, 3, "10.0.0.2_9001", now)
	c.parts["10.0.0.2_9001"] = twin
	_, files := collect(t, m)
	var index MemberIndex
	_ = json.Unmarshal([]byte(files["members/index.json"]), &index)
	names := map[string]bool{}
	for _, member := range index.Members {
		if names[member.Name] || !member.Collected {
			t.Fatalf("members = %+v", index.Members)
		}
		names[member.Name] = true
		if _, ok := files["members/"+member.Name+"/member.json"]; !ok {
			t.Fatalf("%s has no member.json", member.Name)
		}
	}
}

// A member that never answers is given up on at the bound of one member's
// answer, with the collection's own context still alive.
func TestASilentMemberIsGivenUpOnAtTheMembersOwnBound(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	lowerBound(t, &memberAskTimeout, 150*time.Millisecond)
	m, c := clusterFixture(t, now)
	c.hang["10.0.0.3:9001"] = true
	began := time.Now()
	b, files := collect(t, m) // context.Background: only the member's bound ends the wait
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("the collection waited %v for a silent member", took)
	}
	var index MemberIndex
	_ = json.Unmarshal([]byte(files["members/index.json"]), &index)
	if got := index.Members[2]; got.Collected || got.Missing != "timeout" || !index.Members[1].Collected || b.Complete == nil || *b.Complete {
		t.Fatalf("index = %+v", index)
	}
}

// An index that cannot be written is reported, and the collection stands.
func TestAMembersIndexThatCannotBeWrittenIsReported(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m, c := clusterFixture(t, now)
	// A member whose directory takes the index's own name.
	c.members[2].Address = "index.json"
	c.parts["index.json"] = memberFixture(t, 3, "index.json", now)
	b, files := collect(t, m)
	it := itemsByPath(b)["members/index.json"]
	if it.Status != proto.CheckFail || it.Code != messages.CheckDiagnosticsItemStageFailed || it.Evidence == "" {
		t.Fatalf("index item = %+v, want the failed write reported", it)
	}
	if _, ok := files["members/10.0.0.2_9001/server.log"]; !ok || b.Complete == nil {
		t.Fatal("the collection lost its members with the index")
	}
}

// A member's member.json is another machine's bytes: the names it lists as
// cut become items only when they are a part's own.
func TestAMemberCannotNameItemsThroughItsCutList(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m, c := clusterFixture(t, now)
	c.rewriteFacts = func(f *MemberFacts) {
		f.Truncated = []string{"../../manifest.json", "db/settings.json", MemberRoleEventsFile, strings.Repeat("x", 4096)}
	}
	b, files := collect(t, m)
	var index MemberIndex
	if err := json.Unmarshal([]byte(files["members/index.json"]), &index); err != nil {
		t.Fatal(err)
	}
	for _, member := range index.Members {
		if member.Self {
			continue
		}
		if strings.Join(member.Truncated, ",") != MemberRoleEventsFile {
			t.Fatalf("%s truncated = %v, want the role records alone", member.Name, member.Truncated)
		}
	}
	for _, it := range b.Items {
		if strings.Contains(it.Path, "..") || strings.Contains(it.Path, "xxxx") || strings.HasSuffix(it.Path, "/db/settings.json") {
			t.Fatalf("a member named the item %q", it.Path)
		}
	}
	if it := itemsByPath(b)["members/10.0.0.2_9001/role-events.log"]; it.Status != proto.CheckWarn {
		t.Fatalf("role records item = %+v", it)
	}
}
