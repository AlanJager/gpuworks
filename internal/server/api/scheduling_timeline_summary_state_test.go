package api

import (
	"encoding/json"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// part is a stretch of a deployment in one state inside a slice: from and to
// are offsets from the slice's start, level the sleep level (0: none named).
type part struct {
	state    string
	level    int
	from, to time.Duration
}

const sec = time.Second

// stateSlices lays each case in a slice of its own (a minute each, one
// deployment per case, ids 1, 2, ...) before the cap's worth of later
// stretches, and returns the summaries.
func stateSlices(t *testing.T, cases [][]part) []dto.TimelineSummary {
	t.Helper()
	var segs []dto.TimelineSegment
	for i, parts := range cases {
		for _, p := range parts {
			s := seg(uint(i+1), p.state, time.Duration(i)*time.Minute+p.from, time.Duration(i)*time.Minute+p.to)
			s.Level = p.level
			segs = append(segs, s)
		}
	}
	segs = append(segs, swaps(time.Duration(len(cases))*time.Minute, time.Minute, timelineSegmentCap)...)
	got := summarizeSegments(segs, summaryFrom)
	if len(got.summary) != len(cases) {
		t.Fatalf("%d summaries, want %d (a minute each)", len(got.summary), len(cases))
	}
	return got.summary
}

// A deployment's state in a slice is the one it spent the most time in; on a
// tie awake wins over switching over starting over asleep.
func TestSummarizeNamesTheDominantStateOfEachDeployment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []part
		want  string
	}{
		{"awake longest", []part{{"awake", 0, 0, 30 * sec}, {"asleep", 1, 30 * sec, 50 * sec}, {"starting", 0, 50 * sec, 60 * sec}}, "awake"},
		{"asleep longest", []part{{"awake", 0, 0, 10 * sec}, {"asleep", 1, 10 * sec, 50 * sec}, {"switching", 0, 50 * sec, 60 * sec}}, "asleep"},
		{"switching longest", []part{{"awake", 0, 0, 10 * sec}, {"switching", 0, 10 * sec, 40 * sec}, {"asleep", 1, 40 * sec, 60 * sec}}, "switching"},
		{"starting longest", []part{{"starting", 0, 0, 40 * sec}, {"awake", 0, 40 * sec, 60 * sec}}, "starting"},
		{"tie awake asleep", []part{{"asleep", 1, 0, 30 * sec}, {"awake", 0, 30 * sec, 60 * sec}}, "awake"},
		{"tie awake switching", []part{{"switching", 0, 0, 30 * sec}, {"awake", 0, 30 * sec, 60 * sec}}, "awake"},
		{"tie awake starting", []part{{"starting", 0, 0, 30 * sec}, {"awake", 0, 30 * sec, 60 * sec}}, "awake"},
		{"tie switching starting", []part{{"starting", 0, 0, 30 * sec}, {"switching", 0, 30 * sec, 60 * sec}}, "switching"},
		{"tie switching asleep", []part{{"asleep", 2, 0, 30 * sec}, {"switching", 0, 30 * sec, 60 * sec}}, "switching"},
		{"tie starting asleep", []part{{"asleep", 2, 0, 30 * sec}, {"starting", 0, 30 * sec, 60 * sec}}, "starting"},
		{"four-way tie", []part{{"asleep", 1, 0, 15 * sec}, {"starting", 0, 15 * sec, 30 * sec}, {"switching", 0, 30 * sec, 45 * sec}, {"awake", 0, 45 * sec, 60 * sec}}, "awake"},
		// Both round to 30 s on the wire; the unrounded time decides.
		{"closer than a second", []part{{"awake", 0, 0, 29600 * time.Millisecond}, {"asleep", 1, 29600 * time.Millisecond, 60 * sec}}, "asleep"},
		// Stretches of one state add up: 24 s of switching in two stretches beat 20 s awake.
		{"stretches add up", []part{{"switching", 0, 0, 12 * sec}, {"awake", 0, 12 * sec, 32 * sec}, {"switching", 0, 32 * sec, 44 * sec}}, "switching"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := stateSlices(t, [][]part{tc.parts})[0].Deployments[0]
			if d.State != tc.want {
				t.Fatalf("state = %q, want %q (%+v)", d.State, tc.want, d)
			}
		})
	}
}

// Deployments sharing a slice each get their own state.
func TestSummarizeGivesEachDeploymentInASliceItsOwnState(t *testing.T) {
	m := time.Minute
	segs := append([]dto.TimelineSegment{
		seg(1, "awake", 0, 40*sec), seg(1, "asleep", 40*sec, m),
		seg(2, "awake", 0, 20*sec), seg(2, "asleep", 20*sec, m),
	}, swaps(m, m, timelineSegmentCap)...)
	got := summarizeSegments(segs, summaryFrom).summary
	if len(got) != 1 || len(got[0].Deployments) != 2 {
		t.Fatalf("summaries = %+v", got)
	}
	states := map[uint]string{}
	for _, d := range got[0].Deployments {
		states[d.DeploymentID] = d.State
	}
	if states[1] != "awake" || states[2] != "asleep" {
		t.Fatalf("states = %v, want 1 awake and 2 asleep", states)
	}
}

// An asleep deployment carries the sleep level with most asleep time among
// those the segments name; the shallower on a tie, none when no stretch names
// one, and none for any other state.
func TestSummarizeNamesTheSleepLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []part
		state string
		level int
	}{
		{"one level", []part{{"asleep", 1, 0, 60 * sec}}, "asleep", 1},
		{"deeper has more", []part{{"asleep", 1, 0, 20 * sec}, {"asleep", 2, 20 * sec, 50 * sec}}, "asleep", 2},
		{"shallower has more", []part{{"asleep", 2, 0, 20 * sec}, {"asleep", 1, 20 * sec, 50 * sec}}, "asleep", 1},
		{"tie takes the shallower", []part{{"asleep", 2, 0, 25 * sec}, {"asleep", 1, 25 * sec, 50 * sec}}, "asleep", 1},
		{"unknown level does not compete", []part{{"asleep", 0, 0, 40 * sec}, {"asleep", 2, 40 * sec, 50 * sec}}, "asleep", 2},
		{"no level known", []part{{"asleep", 0, 0, 60 * sec}}, "asleep", 0},
		{"awake has none", []part{{"awake", 0, 0, 40 * sec}, {"asleep", 2, 40 * sec, 60 * sec}}, "awake", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := stateSlices(t, [][]part{tc.parts})[0].Deployments[0]
			if d.State != tc.state || d.Level != tc.level {
				t.Fatalf("state %q level %d, want %q level %d", d.State, d.Level, tc.state, tc.level)
			}
		})
	}
}

// Two instances asleep at one level at once count once, as the seconds do:
// 25 s of level 1 on two instances is 25 s, not the 50 s that would outweigh
// the 30 s of level 2.
func TestSummarizeCountsOverlappingAsleepLevelsOnce(t *testing.T) {
	m := time.Minute
	a := seg(1, "asleep", 0, 25*sec)
	a.Level = 1
	b := seg(1, "asleep", 0, 25*sec)
	b.Instance, b.Level = "other", 1
	c := seg(1, "asleep", 30*sec, m)
	c.Level = 2
	segs := append([]dto.TimelineSegment{a, b, c}, swaps(m, m, timelineSegmentCap)...)
	got := summarizeSegments(segs, summaryFrom).summary
	if len(got) != 1 {
		t.Fatalf("summaries = %+v", got)
	}
	if dep := got[0].Deployments[0]; dep.State != "asleep" || dep.Level != 2 || dep.AsleepSeconds != 55 {
		t.Fatalf("%+v, want asleep L2 over 55 s", dep)
	}
}

// The wire form: state always, level only when asleep and known, after the
// seconds.
func TestSummaryDeploymentStateWire(t *testing.T) {
	slices := stateSlices(t, [][]part{
		{{"awake", 0, 0, 40 * sec}, {"asleep", 2, 40 * sec, 60 * sec}},
		{{"asleep", 2, 0, 60 * sec}},
		{{"asleep", 0, 0, 60 * sec}},
		{{"starting", 0, 0, 60 * sec}},
	})
	for i, want := range []string{
		`{"deployment_id":1,"deployment":"dep-1","awake_seconds":40,"asleep_seconds":20,"switching_seconds":0,"starting_seconds":0,"state":"awake"}`,
		`{"deployment_id":2,"deployment":"dep-2","awake_seconds":0,"asleep_seconds":60,"switching_seconds":0,"starting_seconds":0,"state":"asleep","level":2}`,
		`{"deployment_id":3,"deployment":"dep-3","awake_seconds":0,"asleep_seconds":60,"switching_seconds":0,"starting_seconds":0,"state":"asleep"}`,
		`{"deployment_id":4,"deployment":"dep-4","awake_seconds":0,"asleep_seconds":0,"switching_seconds":0,"starting_seconds":60,"state":"starting"}`,
	} {
		body, err := json.Marshal(slices[i].Deployments[0])
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Errorf("slice %d\n got  %s\n want %s", i, body, want)
		}
	}
}
