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
	"sync/atomic"
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
	// Clock is the member's clock when it began to answer, Sent this
	// member's clock when the request left and RoundTrip how long the
	// answer's start then took to arrive: together they place the member's
	// clock against this one's. Sent is taken when the request is sent, not
	// when the member was first looked up.
	Clock     time.Time
	Sent      time.Time
	RoundTrip time.Duration
	// Key is the fingerprint of the member key that answered.
	Key string
}

// Why a member that was reached gave no part.
var (
	// ErrMemberRefused: it does not accept this member's key.
	ErrMemberRefused = errors.New("the member refused the request")
	// ErrMemberBusy: it is giving its part to another collection.
	ErrMemberBusy = errors.New("the member is giving its part to another collection")
	// ErrMemberUnsupported: its release has no diagnostics route (a member
	// not yet upgraded).
	ErrMemberUnsupported = errors.New("the member's release does not give a diagnostics part")
	// ErrMemberFailed: it answered with a failure, or as another member.
	ErrMemberFailed = errors.New("the member failed to give its part")
)

// memberBusyWait is how long a busy member is given before the one retry.
var memberBusyWait = 3 * time.Second

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
	// (the member does not accept this member's key), unsupported (its
	// release has no diagnostics route), busy, or failed (it answered with
	// a failure, as another member, or its part ended early); Evidence is
	// the error's own text.
	Missing  string `json:"missing,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// Truncated names the files of the part cut at this side's size bounds.
	Truncated []string `json:"truncated,omitempty"`
	// AnsweredKey is the fingerprint of the member key that answered.
	AnsweredKey string `json:"answered_key,omitempty"`
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
	case errors.Is(err, ErrMemberBusy):
		return "busy"
	case errors.Is(err, ErrMemberUnsupported):
		return "unsupported"
	case errors.Is(err, ErrMemberFailed):
		return "failed"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	}
	return "unreachable"
}

// collectMembers stages the cluster members' parts under members/. log is
// this member's staged log in the window. It answers whether every member's
// part is there; nil on a server that is not a cluster member.
func (m *Manager) collectMembers(ctx context.Context, b Bundle, work, log string, add func(BundleItem, *stagedEntry)) *bool {
	if m.cluster == nil {
		return nil
	}
	members := m.cluster.Members(ctx)
	if len(members) == 0 {
		return nil
	}
	var bundleBudget atomic.Int64
	bundleBudget.Store(MembersBytes)
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
				files, err = m.askMember(ctx, entry, member, dir, b.From, b.To, &partBudget{member: MemberPartBytes, bundle: &bundleBudget})
			}
			for _, f := range files {
				if f.truncated {
					entry.Truncated = append(entry.Truncated, f.name)
				}
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
	local := filepath.Join(work, "members", "index.json")
	raw, err := json.MarshalIndent(index, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(local), 0o700)
	}
	if err == nil {
		err = os.WriteFile(local, raw, 0o600)
	}
	if err != nil {
		add(failedItem("members/index.json", messages.CheckDiagnosticsItemStageFailed, err, 0), nil)
		return &index.Complete
	}
	add(collectedItem("members/index.json", int64(len(raw)), false), &stagedEntry{name: "members/index.json", path: local})
	return &index.Complete
}

// askMember reads another member's part into dir. A busy member is asked
// once more after a short wait. What arrived before a part ended early is
// kept and the member is still recorded as missing.
func (m *Manager) askMember(ctx context.Context, entry *MemberIndexEntry, member ClusterMember, dir string, from, to time.Time, budget *partBudget) ([]memberFile, error) {
	callCtx, cancel := context.WithTimeout(ctx, MemberPartTimeout)
	defer cancel()
	answer, err := m.cluster.OpenMemberPart(callCtx, member, from, to)
	if errors.Is(err, ErrMemberBusy) {
		select {
		case <-time.After(memberBusyWait):
			answer, err = m.cluster.OpenMemberPart(callCtx, member, from, to)
		case <-callCtx.Done():
		}
	}
	if err != nil {
		entry.Missing = missingReason(err)
		return nil, err
	}
	defer answer.Body.Close()
	entry.AnsweredKey = answer.Key
	if !answer.Clock.IsZero() && !answer.Sent.IsZero() {
		clock, trip := answer.Clock, answer.RoundTrip.Milliseconds()
		offset := clock.Sub(answer.Sent.Add(answer.RoundTrip / 2)).Milliseconds()
		entry.MemberClock, entry.RoundTripMs, entry.ClockOffsetMs = &clock, &trip, &offset
	}
	files, err := readMemberPart(answer.Body, dir, budget)
	if err == nil {
		// The part says who built it: it must be the member that was asked.
		var facts MemberFacts
		if facts, err = readMemberFacts(dir); err == nil && (facts.ID != member.ID || facts.Address != member.Address) {
			err = fmt.Errorf("%w: asked member %d at %s, the part is member %d's at %s", ErrMemberFailed, member.ID, member.Address, facts.ID, facts.Address)
		}
	}
	if err != nil {
		entry.Missing = "failed"
		if callCtx.Err() != nil {
			entry.Missing = "timeout"
		}
	}
	return files, err
}
