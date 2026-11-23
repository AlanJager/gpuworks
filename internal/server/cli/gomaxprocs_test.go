package cli

import (
	"bytes"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestMaxProcsDecision(t *testing.T) {
	cases := []struct {
		name        string
		current     int
		operatorSet bool
		limit       int
		want        int
		lower       bool
	}{
		{"many CPUs are capped", 192, false, 32, 32, true},
		{"one above the cap is capped", 33, false, 32, 32, true},
		{"exactly the cap is kept", 32, false, 32, 32, false},
		{"a small host is kept", 8, false, 32, 8, false},
		{"a container limit below the cap is kept", 4, false, 32, 4, false},
		{"an operator's value is kept even when large", 192, true, 32, 192, false},
		{"an operator's small value is kept", 2, true, 32, 2, false},
		{"no cap keeps the default", 192, false, 0, 192, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, lower := maxProcsDecision(tc.current, tc.operatorSet, tc.limit)
			if want != tc.want || lower != tc.lower {
				t.Fatalf("maxProcsDecision(%d, %v, %d) = %d, %v; want %d, %v", tc.current, tc.operatorSet, tc.limit, want, lower, tc.want, tc.lower)
			}
		})
	}
}

// TestCapServerMaxProcsKeepsAnOperatorValue runs the real function: with
// GOMAXPROCS in the environment it neither changes the runtime nor logs.
func TestCapServerMaxProcsKeepsAnOperatorValue(t *testing.T) {
	before := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(before) })
	runtime.GOMAXPROCS(serverMaxProcs + 8)
	t.Setenv("GOMAXPROCS", "40")
	var out bytes.Buffer
	capServerMaxProcs(slog.New(slog.NewTextHandler(&out, nil)))
	if got := runtime.GOMAXPROCS(0); got != serverMaxProcs+8 {
		t.Fatalf("GOMAXPROCS = %d after an operator set it; want it left at %d", got, serverMaxProcs+8)
	}
	if out.Len() != 0 {
		t.Fatalf("logged %q although nothing changed", out.String())
	}
}

// TestCapServerMaxProcsLowersAndSaysSo runs the real function without an
// operator value on a runtime set above the cap.
func TestCapServerMaxProcsLowersAndSaysSo(t *testing.T) {
	before := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(before) })
	runtime.GOMAXPROCS(serverMaxProcs + 8)
	// t.Setenv restores the original value; the variable is then absent.
	t.Setenv("GOMAXPROCS", "")
	if err := os.Unsetenv("GOMAXPROCS"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	capServerMaxProcs(slog.New(slog.NewTextHandler(&out, nil)))
	if got := runtime.GOMAXPROCS(0); got != serverMaxProcs {
		t.Fatalf("GOMAXPROCS = %d; want the cap %d", got, serverMaxProcs)
	}
	line := out.String()
	for _, want := range []string{"gomaxprocs=32", "default=40", "GOMAXPROCS"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
}
