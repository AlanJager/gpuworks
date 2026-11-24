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
	// included: at least one, or an error when the membership could not be
	// read. The collection then says so, and holds this member's files
	// alone. A single server has no Cluster at all.
	Members(ctx context.Context) ([]ClusterMember, error)
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

// MemberAnswer is another member's part as it arrives: what the member
// route's client answers with.
type MemberAnswer struct {
	// Body is the part (WriteMemberPart's stream); the caller reads it
	// within its own bounds and closes it.
	Body io.ReadCloser
	// Clock is the member's clock when it began to answer (zero when it
	// did not say), Sent this member's clock when the request left and
	// RoundTrip how long the answer's start then took to arrive: together
	// they place the member's clock against this one's.
	Clock     time.Time
	Sent      time.Time
	RoundTrip time.Duration
	// Key is the fingerprint of the member key whose certificate answered
	// (the SHA-256 of its public key, as admissions name it).
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

// SetCluster makes collections cover the cluster's members.
func (m *Manager) SetCluster(c Cluster) { m.cluster = c }

// MemberIndex is members/index.json: which members the collection covers,
// which are missing and why, and how each member's clock stood against the
// collecting member's.
type MemberIndex struct {
	// Complete is false when a member's part is missing: the collection then
	// cannot show what that member saw. It says in one word what Members
	// says entry by entry, for whoever opens the file.
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
	Self bool `json:"self"`
	// Missing is empty when the member's part is in the collection.
	// Otherwise it says why not: unreachable, timeout, refused
	// (the member does not accept this member's key), unsupported (its
	// release has no diagnostics route), busy, or failed (it answered with
	// a failure, as another member, or its part ended early); Evidence is
	// the error's own text.
	Missing  string `json:"missing,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// Truncated names what of the part was cut at a size bound, by the
	// member or on this side.
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

// collectMembers stages the cluster members' parts under members/. A member
// whose part is missing leaves a failed item members/<name>, and a
// membership that could not be read a failed item members: a collection
// with neither holds every member's part.
func (m *Manager) collectMembers(ctx context.Context, b Bundle, work string, add func(BundleItem, *stagedEntry)) {
	if m.cluster == nil {
		return
	}
	members, err := m.cluster.Members(ctx)
	if err == nil && len(members) == 0 {
		err = errors.New("the cluster lists no member")
	}
	if err != nil {
		// Like a member that does not answer, this does not fail the
		// collection; without the item it would read as a single server's.
		add(failedItem("members", messages.CheckDiagnosticsItemReadFailed, err, 0), nil)
		m.log.Warn("diagnostics collection could not read the cluster's members", "bundle", b.ID, "err", err)
		// This member's own role records need its log alone. With no
		// membership to name its directory by, they go beside that log.
		if self := m.cluster.Self(); self != nil {
			files, err := stageMemberPart(ctx, filepath.Join(work, "server"), self, b.From, b.To, m.now())
			if err != nil {
				m.log.Warn("diagnostics collection could not read this member's role records", "bundle", b.ID, "err", err)
			}
			for _, f := range files {
				add(collectedItem("server/"+f.name, f.bytes, f.truncated), &stagedEntry{name: "server/" + f.name, path: f.path})
			}
		}
		return
	}
	var bundleBudget atomic.Int64
	bundleBudget.Store(memberBounds.all)
	sort.Slice(members, func(i, j int) bool { return members[i].Address < members[j].Address })
	entries := make([]MemberIndexEntry, len(members))
	taken := map[string]bool{}
	var wg sync.WaitGroup
	for i, member := range members {
		name := memberDirName(member)
		if taken[name] {
			// Two addresses that read alike as a directory name.
			name += "-" + strconv.FormatUint(member.ID, 10)
		}
		taken[name] = true
		entries[i] = MemberIndexEntry{Name: name, ID: strconv.FormatUint(member.ID, 10), Address: member.Address, Role: member.Role, Self: member.Self}
		wg.Add(1)
		go func(entry *MemberIndexEntry, member ClusterMember) {
			defer wg.Done()
			dir := filepath.Join(work, "members", entry.Name)
			var files []memberFile
			var err error
			if member.Self {
				files, err = stageMemberPart(ctx, dir, m.cluster.Self(), b.From, b.To, m.now())
				if err != nil {
					entry.Missing = "failed"
				}
			} else {
				files, err = m.askMember(ctx, entry, member, dir, b.From, b.To, &partBudget{member: memberBounds.part, bundle: &bundleBudget})
			}
			// What the member itself cut (its member.json says) counts like
			// what was cut on this side.
			cut := map[string]bool{}
			if facts, ferr := readMemberFacts(dir); ferr == nil {
				for _, name := range facts.Truncated {
					// Only what a part can cut: another member's bytes do
					// not name the items of this collection.
					if memberCutName(name) {
						cut[name] = true
					}
				}
			}
			for _, f := range files {
				truncated := f.truncated || cut[f.name]
				delete(cut, f.name)
				if truncated {
					entry.Truncated = append(entry.Truncated, f.name)
				}
				add(collectedItem("members/"+entry.Name+"/"+f.name, f.bytes, truncated), &stagedEntry{name: "members/" + entry.Name + "/" + f.name, path: f.path})
			}
			// Cut things that are no single file here (the stall reports as a
			// whole, or the collecting member's log, which is under server/).
			for name := range cut {
				entry.Truncated = append(entry.Truncated, name)
				add(collectedItem("members/"+entry.Name+"/"+name, 0, true), nil)
			}
			sort.Strings(entry.Truncated)
			if err != nil {
				entry.Evidence = err.Error()
				add(failedItem("members/"+entry.Name, messages.CheckDiagnosticsMemberUnavailable, fmt.Errorf("%s: %w", entry.Missing, err), 0), nil)
				m.log.Warn("diagnostics collection misses a cluster member", "bundle", b.ID, "member", member.Address, "reason", entry.Missing, "err", err)
			}
		}(&entries[i], member)
	}
	wg.Wait()
	index := MemberIndex{Complete: true, Members: entries}
	for _, entry := range entries {
		index.Complete = index.Complete && entry.Missing == ""
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
		return
	}
	add(collectedItem("members/index.json", int64(len(raw)), false), &stagedEntry{name: "members/index.json", path: local})
}

// askMember reads another member's part into dir. A busy member is asked
// once more after a short wait. What arrived before a part ended early is
// kept and the member is still recorded as missing.
func (m *Manager) askMember(ctx context.Context, entry *MemberIndexEntry, member ClusterMember, dir string, from, to time.Time, budget *partBudget) ([]memberFile, error) {
	callCtx, cancel := context.WithTimeout(ctx, memberBounds.wait)
	defer cancel()
	answer, err := m.cluster.OpenMemberPart(callCtx, member, from, to)
	if errors.Is(err, ErrMemberBusy) {
		select {
		case <-time.After(memberBounds.busyWait):
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

// memberCutName is a name a member's member.json may list as cut.
func memberCutName(name string) bool {
	switch name {
	case MemberLogFile, MemberRoleEventsFile, MemberLeaderChangesFile, strings.TrimSuffix(memberStallPrefix, "/"):
		return true
	}
	return false
}
