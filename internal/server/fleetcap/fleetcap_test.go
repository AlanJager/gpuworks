package fleetcap

import (
	"reflect"
	"testing"
)

var unlimited = Limit{}

func limit(n int64) Limit { return Limit{Limited: true, Max: n} }

// The vectors of the enrollment-order rule. Each names the caps, the fleet
// and the outcome; they are the rule's contract, kept beside it.
func TestSelectVectors(t *testing.T) {
	zero := limit(0)
	cases := []struct {
		name string
		caps Caps
		in   []Node
		keep []uint
		held []uint
		kept Usage
		used Usage
		full bool
	}{
		{
			name: "fits exactly",
			caps: Caps{limit(3), limit(24)},
			in:   []Node{{1, 8}, {2, 8}, {3, 8}},
			kept: Usage{3, 24}, used: Usage{3, 24},
		},
		{
			name: "one node too many",
			caps: Caps{limit(2), unlimited},
			in:   []Node{{1, 8}, {2, 8}, {3, 8}},
			held: []uint{3}, kept: Usage{2, 16}, used: Usage{3, 24}, full: true,
		},
		{
			name: "one card too many",
			caps: Caps{unlimited, limit(23)},
			in:   []Node{{1, 8}, {2, 8}, {3, 8}},
			held: []uint{3}, kept: Usage{2, 16}, used: Usage{3, 24}, full: true,
		},
		{
			name: "stop at first misfit, later small node is still held",
			caps: Caps{limit(4), limit(20)},
			in:   []Node{{1, 8}, {2, 8}, {3, 8}, {4, 1}},
			held: []uint{3, 4}, kept: Usage{2, 16}, used: Usage{4, 25}, full: true,
		},
		{
			name: "enrollment order from unsorted input; cardless nodes ignored",
			caps: Caps{limit(2), unlimited},
			in:   []Node{{9, 2}, {4, 0}, {5, 1}, {7, 4}, {1, -1}},
			held: []uint{9}, kept: Usage{2, 5}, used: Usage{3, 7}, full: true,
		},
		{
			name: "unlimited",
			caps: Caps{},
			in:   []Node{{1, 100}, {2, 100}},
			kept: Usage{2, 200}, used: Usage{2, 200},
		},
		{
			name: "zero caps: every counted node held",
			caps: Caps{zero, zero},
			in:   []Node{{2, 1}, {1, 1}, {3, 0}},
			held: []uint{1, 2}, used: Usage{2, 2}, full: true,
		},
		{
			name: "first node alone exceeds cards",
			caps: Caps{limit(4), limit(4)},
			in:   []Node{{1, 8}, {2, 2}},
			held: []uint{1, 2}, used: Usage{2, 10}, full: true,
		},
		{name: "no nodes", caps: Caps{limit(1), limit(1)}},
		{
			name: "a kept node survives over a lower id",
			caps: Caps{limit(2), unlimited},
			in:   []Node{{1, 8}, {2, 8}, {3, 8}},
			keep: []uint{3},
			held: []uint{2}, kept: Usage{2, 16}, used: Usage{3, 24}, full: true,
		},
		{
			name: "kept nodes come first by ascending id, whatever the list order",
			caps: Caps{limit(2), unlimited},
			in:   []Node{{1, 8}, {2, 8}, {3, 8}, {4, 8}},
			keep: []uint{4, 3},
			held: []uint{1, 2}, kept: Usage{2, 16}, used: Usage{4, 32}, full: true,
		},
		{
			name: "kept overflow is held like any other node",
			caps: Caps{limit(1), unlimited},
			in:   []Node{{1, 8}, {2, 8}, {3, 8}},
			keep: []uint{2, 3},
			held: []uint{1, 3}, kept: Usage{1, 8}, used: Usage{3, 24}, full: true,
		},
		{
			name: "a kept node that does not fit holds every node after it",
			caps: Caps{unlimited, limit(10)},
			in:   []Node{{1, 4}, {2, 4}, {3, 8}},
			keep: []uint{3},
			held: []uint{1, 2}, kept: Usage{1, 8}, used: Usage{3, 16}, full: true,
		},
		{
			name: "an id naming no node is ignored",
			caps: Caps{limit(2), unlimited},
			in:   []Node{{1, 8}, {2, 8}, {3, 8}},
			keep: []uint{99},
			held: []uint{3}, kept: Usage{2, 16}, used: Usage{3, 24}, full: true,
		},
		{
			name: "a kept node without cards is ignored",
			caps: Caps{limit(1), unlimited},
			in:   []Node{{1, 8}, {2, 0}, {3, 8}},
			keep: []uint{2},
			held: []uint{3}, kept: Usage{1, 8}, used: Usage{2, 16}, full: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Select(tc.caps, tc.in, tc.keep)
			if !reflect.DeepEqual(s.Held, tc.held) || s.Kept != tc.kept || s.Used != tc.used || s.Full != tc.full {
				t.Fatalf("selection = %+v", s)
			}
		})
	}
}

// Without a keep list the selection is the enrollment-order rule exactly: an
// empty list and nil agree on every vector above.
func TestSelectWithoutKeepIsEnrollmentOrder(t *testing.T) {
	caps := Caps{limit(2), limit(20)}
	in := []Node{{5, 8}, {2, 8}, {9, 1}, {3, 8}, {4, 0}}
	a, b := Select(caps, in, nil), Select(caps, in, []uint{})
	want := Selection{Used: Usage{4, 25}, Kept: Usage{2, 16}, Full: true, Held: []uint{5, 9}}
	if !reflect.DeepEqual(a, want) || !reflect.DeepEqual(b, want) {
		t.Fatalf("nil keep = %+v, empty keep = %+v, want %+v", a, b, want)
	}
}

func TestMinTakesTheTighterCap(t *testing.T) {
	for _, tc := range []struct{ a, b, want Limit }{
		{unlimited, unlimited, unlimited},
		{unlimited, limit(3), limit(3)},
		{limit(3), unlimited, limit(3)},
		{limit(5), limit(3), limit(3)},
		{limit(0), limit(3), limit(0)},
	} {
		if got := Min(tc.a, tc.b); got != tc.want {
			t.Fatalf("Min(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if (Caps{}).Limited() || !(Caps{Cards: limit(1)}).Limited() {
		t.Fatal("Limited")
	}
}
