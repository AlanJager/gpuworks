package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Cluster is the control-plane cluster this server is a member of (app
// wires it; a single server has none). A collection made on a member covers
// every member: its own part read here, the others' asked for over the
// member-authenticated route. A member that does not answer is recorded as
// missing; it never fails the collection.
type Cluster interface {
	// Members are the cluster's members as this one knows them, itself
	// included. Empty means this server is not a cluster member.
	Members(ctx context.Context) []ClusterMember
	// Self is where this member's own part is read from.
	Self() MemberSource
	// OpenMemberPart asks another member for its part of the window.
	OpenMemberPart(ctx context.Context, member ClusterMember, from, to time.Time) (MemberAnswer, error)
}

// ClusterMember is one member of the cluster.
type ClusterMember struct {
	ID uint64
	// Address is the member's database address, which names it.
	Address string
	Role    string
	Self    bool
}

// MemberAnswer is another member's part as it arrives.
type MemberAnswer struct {
	// Body is the part (WriteMemberPart's stream).
	Body io.ReadCloser
	// Clock is the member's clock when it began to answer and RoundTrip how
	// long the answer's start took to arrive: together they place the
	// member's clock against this one's.
	Clock     time.Time
	RoundTrip time.Duration
}

// ErrMemberRefused is a member that answered and would not give its part
// (it does not accept this member's key).
var ErrMemberRefused = errors.New("the member refused the request")

// SetCluster makes collections cover the cluster's members.
func (m *Manager) SetCluster(c Cluster) { m.cluster = c }

// MemberIndex is members/index.json: which members the collection covers,
// which are missing and why, and how each member's clock stood against the
// collecting member's.
type MemberIndex struct {
	// Complete is false when a member's part is missing: the collection then
	// cannot show what that member saw.
	Complete bool               `json:"complete"`
	Members  []MemberIndexEntry `json:"members"`
}

// MemberIndexEntry is one member in the index.
type MemberIndexEntry struct {
	// Name is the member's directory under members/.
	Name    string `json:"name"`
	ID      string `json:"id"`
	Address string `json:"address"`
	Role    string `json:"role,omitempty"`
	// Self marks the member that made the collection; its log and stall
	// reports are under server/.
	Self      bool `json:"self"`
	Collected bool `json:"collected"`
	// Missing says why a part is not there: unreachable, timeout, refused
	// or failed (it ended early or could not be kept); Evidence is the
	// error's own text.
	Missing  string `json:"missing,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// MemberClock is the member's clock when it began to answer.
	// ClockOffsetMs is that clock minus the collecting member's at the same
	// moment, known to within half of RoundTripMs: add it to the collecting
	// member's times to read them on this member's clock.
	MemberClock   *time.Time `json:"member_clock,omitempty"`
	ClockOffsetMs *int64     `json:"clock_offset_ms,omitempty"`
	RoundTripMs   *int64     `json:"round_trip_ms,omitempty"`
}

// memberDirName is a member's directory name: its address with anything
// that does not belong in a path replaced.
func memberDirName(member ClusterMember) string {
	var b strings.Builder
	for _, r := range member.Address {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if name := strings.Trim(b.String(), "._"); name != "" {
		return name
	}
	return "member-" + strconv.FormatUint(member.ID, 10)
}

// missingReason sorts a failed ask.
func missingReason(err error) string {
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, ErrMemberRefused):
		return "refused"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	}
	return "unreachable"
}

// collectMembers stages the cluster members' parts under members/. log is
// this member's staged log in the window.
func (m *Manager) collectMembers(ctx context.Context, b Bundle, work, log string, add func(BundleItem, *stagedEntry)) {
	if m.cluster == nil {
		return
	}
	members := m.cluster.Members(ctx)
	if len(members) == 0 {
		return
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Address < members[j].Address })
	entries := make([]MemberIndexEntry, len(members))
	var wg sync.WaitGroup
	for i, member := range members {
		name := memberDirName(member)
		entries[i] = MemberIndexEntry{Name: name, ID: strconv.FormatUint(member.ID, 10), Address: member.Address, Role: member.Role, Self: member.Self}
		wg.Add(1)
		go func(entry *MemberIndexEntry, member ClusterMember) {
			defer wg.Done()
			dir := filepath.Join(work, "members", entry.Name)
			var files []memberFile
			var err error
			if member.Self {
				files, err = stageMemberPart(ctx, dir, m.cluster.Self(), b.From, b.To, m.now(), log)
				if err != nil {
					entry.Missing = "failed"
				}
			} else {
				files, err = m.askMember(ctx, entry, member, dir, b.From, b.To)
			}
			for _, f := range files {
				add(collectedItem("members/"+entry.Name+"/"+f.name, f.bytes, f.truncated), &stagedEntry{name: "members/" + entry.Name + "/" + f.name, path: f.path})
			}
			if err != nil {
				entry.Evidence = err.Error()
				add(failedItem("members/"+entry.Name, messages.CheckDiagnosticsMemberUnavailable, fmt.Errorf("%s: %w", entry.Missing, err), 0), nil)
				m.log.Warn("diagnostics collection misses a cluster member", "bundle", b.ID, "member", member.Address, "reason", entry.Missing, "err", err)
				return
			}
			entry.Collected = true
		}(&entries[i], member)
	}
	wg.Wait()
	index := MemberIndex{Complete: true, Members: entries}
	for _, entry := range entries {
		index.Complete = index.Complete && entry.Collected
	}
	raw, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		add(failedItem("members/index.json", messages.CheckDiagnosticsItemStageFailed, err, 0), nil)
		return
	}
	local := filepath.Join(work, "members", "index.json")
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err == nil {
		err = os.WriteFile(local, raw, 0o600)
	}
	if err != nil {
		add(failedItem("members/index.json", messages.CheckDiagnosticsItemStageFailed, err, 0), nil)
		return
	}
	add(collectedItem("members/index.json", int64(len(raw)), false), &stagedEntry{name: "members/index.json", path: local})
}

// askMember reads another member's part into dir. What arrived before a
// part ended early is kept and the member is still recorded as missing.
func (m *Manager) askMember(ctx context.Context, entry *MemberIndexEntry, member ClusterMember, dir string, from, to time.Time) ([]memberFile, error) {
	callCtx, cancel := context.WithTimeout(ctx, MemberPartTimeout)
	defer cancel()
	asked := m.now()
	answer, err := m.cluster.OpenMemberPart(callCtx, member, from, to)
	if err != nil {
		entry.Missing = missingReason(err)
		return nil, err
	}
	defer answer.Body.Close()
	if !answer.Clock.IsZero() {
		clock, trip := answer.Clock, answer.RoundTrip.Milliseconds()
		offset := clock.Sub(asked.Add(answer.RoundTrip / 2)).Milliseconds()
		entry.MemberClock, entry.RoundTripMs, entry.ClockOffsetMs = &clock, &trip, &offset
	}
	files, err := readMemberPart(answer.Body, dir)
	if err != nil {
		entry.Missing = "failed"
		if callCtx.Err() != nil {
			entry.Missing = "timeout"
		}
	}
	return files, err
}
