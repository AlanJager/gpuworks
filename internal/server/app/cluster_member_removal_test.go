package app

import (
	"errors"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// removalMembers builds a membership from one letter per member, with IDs
// from 1: V a voter that answers, v a voter that does not, S a spare that
// answers, s a spare that does not, B a stand-by that answers.
func removalMembers(t *testing.T, layout string) ([]cluster.Member, func(cluster.Member) bool) {
	t.Helper()
	var members []cluster.Member
	answers := map[uint64]bool{}
	for i, letter := range layout {
		id := uint64(i + 1)
		role := map[rune]string{'V': "voter", 'v': "voter", 'S': "spare", 's': "spare", 'B': "stand-by"}[letter]
		if role == "" {
			t.Fatalf("unknown member letter %q", letter)
		}
		members = append(members, cluster.Member{ID: id, Address: "10.0.0." + string(rune('0'+id)) + ":9001", Role: role})
		answers[id] = letter == 'V' || letter == 'S' || letter == 'B'
	}
	return members, func(member cluster.Member) bool { return answers[member.ID] }
}

func TestMemberRemovalRefusal(t *testing.T) {
	for _, c := range []struct {
		name   string
		layout string
		remove uint64
		force  bool
		want   error
	}{
		// Four members: three remain.
		{"spare of four", "VVVS", 4, false, nil},
		{"voter of four with a spare to promote", "VVVS", 1, false, nil},
		{"offline voter of four", "VVvS", 3, false, nil},
		{"voter of four leaves one answering voter", "VVvS", 1, false, ErrRemovalBreaksQuorum},
		{"voter of four leaves one answering voter, forced", "VVvS", 1, true, ErrRemovalBreaksQuorum},
		{"spare of four while one voter answers", "Vvvs", 4, false, ErrRemovalBreaksQuorum},
		{"spare of four while two of three voters answer", "VVvS", 4, false, nil},
		// Three members: two would remain.
		{"voter of three", "VVV", 3, false, ErrRemovalLeavesTooFewMembers},
		{"voter of three, forced", "VVV", 3, true, nil},
		{"offline voter of three", "VVv", 3, false, ErrRemovalLeavesTooFewMembers},
		{"offline voter of three, forced", "VVv", 3, true, nil},
		{"answering voter of three beside an offline one", "VVv", 1, false, ErrRemovalBreaksQuorum},
		{"answering voter of three beside an offline one, forced", "VVv", 1, true, ErrRemovalBreaksQuorum},
		{"offline spare of three with two voters", "VVs", 3, false, ErrRemovalLeavesTooFewMembers},
		{"offline spare of three with two voters, forced", "VVs", 3, true, nil},
		{"answering spare of three with two voters", "VVS", 3, false, ErrRemovalLeavesTooFewMembers},
		{"stand-by of three", "VVB", 3, false, ErrRemovalLeavesTooFewMembers},
		// Two members: one would remain.
		{"spare of two", "VS", 2, false, ErrRemovalBreaksQuorum},
		{"spare of two, forced", "VS", 2, true, nil},
		{"offline spare of two, forced", "Vs", 2, true, nil},
		{"only voter of two", "VS", 1, false, ErrRemovalBreaksQuorum},
		{"only voter of two, forced", "VS", 1, true, ErrRemovalBreaksQuorum},
		{"voter of two voters", "VV", 2, false, ErrRemovalBreaksQuorum},
		{"voter of two voters, forced", "VV", 2, true, nil},
		{"answering voter of two beside an offline one, forced", "Vv", 1, true, ErrRemovalBreaksQuorum},
		// The last member never leaves.
		{"only member", "V", 1, false, ErrRemovalBreaksQuorum},
		{"only member, forced", "V", 1, true, ErrRemovalBreaksQuorum},
	} {
		t.Run(c.name, func(t *testing.T) {
			members, online := removalMembers(t, c.layout)
			before := slices.Clone(members)
			if err := memberRemovalRefusal(members, online, c.remove, c.force); !errors.Is(err, c.want) || (c.want == nil && err != nil) {
				t.Fatalf("removal of %d from %s (force %t) = %v, want %v", c.remove, c.layout, c.force, err, c.want)
			}
			if !slices.Equal(members, before) {
				t.Fatal("the decision changed the membership it was given")
			}
		})
	}
}

func TestMemberRemovalRefusalNeedsATargetAndAnObservation(t *testing.T) {
	members, online := removalMembers(t, "VVVS")
	if err := memberRemovalRefusal(members, online, 9, true); err == nil || errors.Is(err, ErrRemovalBreaksQuorum) || errors.Is(err, ErrRemovalLeavesTooFewMembers) {
		t.Fatalf("unknown member = %v", err)
	}
	// Without an observation every member would have to be assumed answering.
	if err := memberRemovalRefusal(members, nil, 4, true); err == nil {
		t.Fatal("removal was allowed without an observation of which members answer")
	}
	asked := map[uint64]bool{}
	if err := memberRemovalRefusal(members, func(member cluster.Member) bool { asked[member.ID] = true; return true }, 1, false); err != nil {
		t.Fatal(err)
	}
	// The member that leaves and members that do not vote are not asked.
	if asked[1] || asked[4] || !asked[2] || !asked[3] {
		t.Fatalf("observed members %v", asked)
	}
}

func TestMemberReplacementRefusal(t *testing.T) {
	for _, c := range []struct {
		name        string
		layout      string
		old, next   uint64
		force       bool
		wantPromote bool
		want        error
	}{
		{"successor already votes among four voters", "VVVV", 1, 4, false, false, nil},
		{"spare successor of four", "VVVS", 1, 4, false, true, nil},
		{"spare successor replaces the offline voter", "VVvS", 3, 4, false, true, nil},
		{"offline successor still leaves two answering voters", "VVVs", 1, 4, false, true, nil},
		{"offline successor beside an offline voter", "VVvs", 1, 4, false, false, ErrRemovalBreaksQuorum},
		{"successor inside a cluster of three", "VVS", 1, 3, false, false, ErrRemovalLeavesTooFewMembers},
		{"successor inside a cluster of three, forced", "VVS", 1, 3, true, true, nil},
		{"single voter cannot accept a promotion", "VSS", 1, 2, true, false, ErrRemovalBreaksQuorum},
	} {
		t.Run(c.name, func(t *testing.T) {
			members, online := removalMembers(t, c.layout)
			before := slices.Clone(members)
			promote, err := memberReplacementRefusal(members, online, c.old, c.next, c.force)
			if !errors.Is(err, c.want) || (c.want == nil && err != nil) || promote != c.wantPromote {
				t.Fatalf("replace %d by %d in %s (force %t) = promote %t, %v; want promote %t, %v", c.old, c.next, c.layout, c.force, promote, err, c.wantPromote, c.want)
			}
			if !slices.Equal(members, before) {
				t.Fatal("the decision changed the membership it was given")
			}
		})
	}
	members, online := removalMembers(t, "VVVS")
	for _, pair := range [][2]uint64{{1, 1}, {4, 1}, {1, 9}, {9, 4}} {
		if promote, err := memberReplacementRefusal(members, online, pair[0], pair[1], true); err == nil || promote {
			t.Fatalf("replacement %v was accepted", pair)
		}
	}
}
