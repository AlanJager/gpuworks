package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
)

// loggedMember is a member whose server log is the file a test wrote.
type loggedMember struct{ log string }

func (m loggedMember) Facts(context.Context) diagnostics.MemberFacts {
	return diagnostics.MemberFacts{}
}
func (m loggedMember) LogFiles() []string            { return []string{m.log} }
func (m loggedMember) StallDir() string              { return filepath.Dir(m.log) }
func (m loggedMember) Status(context.Context) string { return "" }

// A diagnostics collection picks the role records out of a member's server
// log by their message. Nothing else ties the two together, so this runs the
// tracker that writes them through the collection's own part: rewording one
// of its messages without the collection following fails here, instead of
// leaving role-events.log or leader-changes.log silently empty.
func TestTheDiagnosticsPartFindsEveryRecordTheTrackerWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(file, nil))
	tracker := newRoleTracker(func() *slog.Logger { return logger })
	// A follower that campaigns in vain and returns to its leader, then a
	// flood past the minute's budget: every message the tracker has.
	for _, record := range []string{
		at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=4 id=2 address=b:9001",
		at(600) + " event=following leader=3 leader_address=c:9001 term=4",
		at(3000) + " event=loop_late late_ms=2152 period_ms=75",
		at(3000) + " event=election_start reason=election_timeout term=4 leader=3 since_leader_contact_ms=2221 timeout_ms=1143",
		at(3000) + " event=state old=FOLLOWER new=CANDIDATE term=4 id=2 address=b:9001",
		at(3006) + " event=state old=CANDIDATE new=FOLLOWER term=4 id=2 address=b:9001",
		at(3007) + " event=following leader=3 leader_address=c:9001 term=4",
	} {
		tracker.record(record)
	}
	for i := 0; i < roleRecordsPerMinute+10; i++ {
		tracker.record(at(4000) + " event=never_heard_of x=1")
	}
	// The budget's report is written with the first record of the next minute.
	tracker.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	tracker.record(at(70000) + " event=never_heard_of x=2")
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The records above make the tracker write each of its four messages.
	// They are counted, not named: naming them here would only repeat the
	// text the collection's filter already holds.
	messages := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(written)), "\n") {
		_, rest, _ := strings.Cut(line, `msg="`)
		message, _, _ := strings.Cut(rest, `"`)
		messages[message] = true
	}
	if len(messages) != 4 {
		t.Fatalf("the tracker wrote %d kinds of line, want its four: %v", len(messages), messages)
	}

	var part bytes.Buffer
	now := time.Now()
	if err := diagnostics.WriteMemberPart(context.Background(), &part, loggedMember{path}, now.Add(-time.Hour), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&part)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for tr := tar.NewReader(gz); ; {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		files[h.Name] = string(body)
	}
	roles, leaders := files[diagnostics.MemberRoleEventsFile], files[diagnostics.MemberLeaderChangesFile]
	// Every line the tracker wrote is in one of the two extracts.
	if got, want := strings.Count(roles, "\n")+strings.Count(leaders, "\n"), bytes.Count(written, []byte("\n")); got != want {
		t.Fatalf("the part's extracts hold %d of the tracker's %d lines: the filter in internal/server/diagnostics does not know one of the tracker's messages\nrole-events.log:\n%s\nleader-changes.log:\n%s", got, want, roles, leaders)
	}
	// The leader's two summaries are apart from the events.
	if strings.Count(leaders, "\n") != 2 || !strings.Contains(leaders, "new_leader=3") || !strings.Contains(roles, "event=election_start") {
		t.Fatalf("role-events.log:\n%s\nleader-changes.log:\n%s", roles, leaders)
	}
}
