package enginelog_test

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// Tail keeps the last non-empty lines in the order they were printed,
// without terminal control sequences, each within the bound of a line that
// stands in a failure message.
func TestTailKeepsTheLastNonEmptyLinesBounded(t *testing.T) {
	long := strings.Repeat("é", 400)
	for _, tc := range []struct {
		name string
		log  string
		n    int
		want []string
	}{
		{"empty", "", 3, nil},
		{"blank lines only", "\n  \n\t\n", 3, nil},
		{"fewer than n", "one\ntwo\n", 3, []string{"one", "two"}},
		{"last n in order", "one\ntwo\n\nthree\r\nfour\n\n", 3, []string{"two", "three", "four"}},
		{"control sequences", "\x1b[31mThis recipe requires TP16 across two nodes.\x1b[0m\n", 3, []string{"This recipe requires TP16 across two nodes."}},
		{"long line", long + "\n", 1, []string{strings.Repeat("é", 300) + "…"}},
		{"progress redrawn with carriage returns", "Loading: 10%|#\rLoading: 20%|##\rLoading: 30%|###\n", 2, []string{"Loading: 20%|##", "Loading: 30%|###"}},
		{"a long redrawn bar keeps its newest state", strings.Repeat("Loading shard: 1%\r", 200) + "Loading shard: 99%\rKilled\n", 2, []string{"Loading shard: 99%", "Killed"}},
		{"zero", "one\n", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := enginelog.Tail(tc.log, tc.n); !slices.Equal(got, tc.want) {
				t.Fatalf("Tail = %q, want %q", got, tc.want)
			}
		})
	}
}

// A line printed several times in a row is one line of a distinct tail; the
// same line coming back after another is kept, and Tail itself still repeats.
func TestTailDistinctCountsARepeatedLineOnce(t *testing.T) {
	const out = "loading weights\nwaiting for peers\nwaiting for peers\n\nwaiting for peers\r\nstill here\nwaiting for peers\nwaiting for peers\n"
	if got, want := enginelog.TailDistinct(out, 3), []string{"waiting for peers", "still here", "waiting for peers"}; !slices.Equal(got, want) {
		t.Fatalf("distinct tail = %q, want %q", got, want)
	}
	if got, want := enginelog.TailDistinct(out, 10), []string{"loading weights", "waiting for peers", "still here", "waiting for peers"}; !slices.Equal(got, want) {
		t.Fatalf("whole distinct tail = %q, want %q", got, want)
	}
	if got, want := enginelog.Tail(out, 3), []string{"still here", "waiting for peers", "waiting for peers"}; !slices.Equal(got, want) {
		t.Fatalf("tail = %q, want %q", got, want)
	}
	if got := enginelog.TailDistinct("", 3); got != nil {
		t.Fatalf("empty output = %q", got)
	}
}
