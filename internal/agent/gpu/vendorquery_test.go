package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// queryTimings shortens the query clocks for one test and gives it an empty
// query table.
func queryTimings(t *testing.T, wait, limit, maxAge time.Duration) {
	t.Helper()
	oldWait, oldLimit, oldAge := vendorCLITimeout, vendorQueryLimit, vendorQueryMaxAge
	vendorCLITimeout, vendorQueryLimit, vendorQueryMaxAge = wait, limit, maxAge
	reset := func() {
		vendorQueries.Lock()
		vendorQueries.byKey = map[string]*vendorQuery{}
		vendorQueries.binaries = map[string]*vendorBinary{}
		vendorQueries.Unlock()
	}
	reset()
	t.Cleanup(func() {
		// Background queries read the clocks; let them finish first.
		waitIdle(t, "")
		vendorCLITimeout, vendorQueryLimit, vendorQueryMaxAge = oldWait, oldLimit, oldAge
		reset()
	})
}

// countingCLI installs a fake CLI that logs each start and prints its start
// number, sleeping for the duration in the file "delay" (seconds, default 0).
func countingCLI(t *testing.T, name string) (dir string, starts func() int) {
	t.Helper()
	dir = fakeCLI(t, name, `d="$(dirname "$0")"
echo "start $* $(date +%s.%N)" >> "$d/starts"
n=$(grep -c . "$d/starts")
[ -e "$d/delay" ] && sleep "$(cat "$d/delay")"
[ -e "$d/fail" ] && { echo "boom"; exit 3; }
echo "answer $n"
`)
	return dir, func() int {
		raw, _ := os.ReadFile(filepath.Join(dir, "starts"))
		return strings.Count(string(raw), "start ")
	}
}

func setDelay(t *testing.T, dir, seconds string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "delay"), []byte(seconds), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitIdle waits until no query of name (of any CLI when empty) runs.
func waitIdle(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		vendorQueries.Lock()
		busy := false
		for key, q := range vendorQueries.byKey {
			if (name == "" || strings.HasPrefix(key, name+"\x00")) && q.running != nil {
				busy = true
			}
		}
		vendorQueries.Unlock()
		if !busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s queries never finished", name)
}

func TestQueryVendorCLIAnswersFreshWhileTheDriverIsQuick(t *testing.T) {
	queryTimings(t, 2*time.Second, 10*time.Second, time.Minute)
	_, starts := countingCLI(t, "gw-quick-smi")
	for want := 1; want <= 2; want++ {
		out, err := QueryVendorCLI(t.Context(), "gw-quick-smi", "q")
		if err != nil || out != "answer "+string(rune('0'+want))+"\n" || starts() != want {
			t.Fatalf("call %d: out=%q err=%v starts=%d; each call must run its own query", want, out, err, starts())
		}
	}
}

// A slow query outlives its caller instead of being killed; the next caller
// gets its answer while a new query runs, and concurrent callers share one
// process.
func TestQueryVendorCLIServesTheLastAnswerOfASlowDriver(t *testing.T) {
	queryTimings(t, 100*time.Millisecond, 10*time.Second, time.Minute)
	dir, starts := countingCLI(t, "gw-slow-q-smi")
	setDelay(t, dir, "0.4")

	start := time.Now()
	if _, err := QueryVendorCLI(t.Context(), "gw-slow-q-smi", "q"); !IsCLIUnresponsive(err) {
		t.Fatalf("first call without an earlier answer: err = %v", err)
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("caller waited %s for a slow query", took)
	}
	waitIdle(t, "gw-slow-q-smi")
	firstDone := time.Now()

	ctx, answered := WithResultTime(t.Context())
	var wg sync.WaitGroup
	outs := make([]string, 5)
	errs := make([]error, 5)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = QueryVendorCLI(ctx, "gw-slow-q-smi", "q")
		}()
	}
	wg.Wait()
	for i := range outs {
		if errs[i] != nil || outs[i] != "answer 1\n" {
			t.Fatalf("caller %d: out=%q err=%v; want the finished first answer", i, outs[i], errs[i])
		}
	}
	if at := answered.Oldest(); at.IsZero() || at.After(firstDone) {
		t.Fatalf("answer time = %s, want when the first query finished (by %s)", at, firstDone)
	}
	waitIdle(t, "gw-slow-q-smi")
	if starts() != 2 {
		t.Fatalf("starts = %d; five concurrent callers must share one new query", starts())
	}
	if out, err := QueryVendorCLI(t.Context(), "gw-slow-q-smi", "q"); err != nil || out != "answer 2\n" {
		t.Fatalf("after the second query finished: out=%q err=%v", out, err)
	}
}

func TestQueryVendorCLIStopsServingAnAnswerPastItsAge(t *testing.T) {
	// The first call only primes an answer, so it may wait as long as a
	// loaded runner needs to start the fake CLI; the 100 ms wait applies to the
	// slow calls after it. The age is long enough that the young answer is
	// still young by the time the second call has waited.
	queryTimings(t, 10*time.Second, 10*time.Second, time.Second)
	dir, _ := countingCLI(t, "gw-aged-smi")
	if _, err := QueryVendorCLI(t.Context(), "gw-aged-smi", "q"); err != nil {
		t.Fatal(err)
	}
	vendorCLITimeout = 100 * time.Millisecond
	setDelay(t, dir, "2")
	if out, err := QueryVendorCLI(t.Context(), "gw-aged-smi", "q"); err != nil || out != "answer 1\n" {
		t.Fatalf("young answer: out=%q err=%v", out, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := QueryVendorCLI(t.Context(), "gw-aged-smi", "q"); !IsCLIUnresponsive(err) {
		t.Fatalf("answer past its age: err = %v, want unresponsive", err)
	}
}

// A query that finished with an error reports it as is, even when an earlier
// answer exists: only slowness is covered by the last answer.
func TestQueryVendorCLIReportsAFinishedFailure(t *testing.T) {
	queryTimings(t, time.Second, 10*time.Second, time.Minute)
	dir, _ := countingCLI(t, "gw-failing-smi")
	if _, err := QueryVendorCLI(t.Context(), "gw-failing-smi", "q"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := QueryVendorCLI(t.Context(), "gw-failing-smi", "q")
	var exitErr interface{ ExitCode() int }
	if err == nil || IsCLIUnresponsive(err) || !strings.Contains(out, "boom") {
		t.Fatalf("finished failure: out=%q err=%v", out, err)
	}
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("err = %v, want the exit status", err)
	}
}

// The background query is killed at its own limit, and while one query of a
// binary runs, another query of it waits for its turn instead of joining the
// driver's queue.
func TestQueryVendorCLIRunsOneQueryPerBinaryAndKillsAtTheLimit(t *testing.T) {
	queryTimings(t, 50*time.Millisecond, 300*time.Millisecond, time.Minute)
	dir, starts := countingCLI(t, "gw-limit-smi")
	setDelay(t, dir, "5")
	start := time.Now()
	if _, err := QueryVendorCLI(t.Context(), "gw-limit-smi", "a"); !IsCLIUnresponsive(err) {
		t.Fatalf("a: err = %v", err)
	}
	if _, err := QueryVendorCLI(t.Context(), "gw-limit-smi", "b"); !IsCLIUnresponsive(err) {
		t.Fatalf("b: err = %v", err)
	}
	if starts() != 1 {
		t.Fatalf("starts while a runs = %d; b must wait for the binary's slot", starts())
	}
	waitIdle(t, "gw-limit-smi")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("queries took %s; the limit must kill them", took)
	}
	if starts() != 2 {
		t.Fatalf("starts = %d, want a then b", starts())
	}
	vendorQueries.Lock()
	err := vendorQueries.byKey["gw-limit-smi\x00a"].err
	vendorQueries.Unlock()
	if !IsCLIUnresponsive(err) || !strings.Contains(err.Error(), "within 300ms") {
		t.Fatalf("killed query recorded err = %v", err)
	}
}

func TestQueryVendorCLICallerCancellation(t *testing.T) {
	queryTimings(t, time.Second, 10*time.Second, time.Minute)
	dir, _ := countingCLI(t, "gw-cancel-smi")
	setDelay(t, dir, "0.5")
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := QueryVendorCLI(ctx, "gw-cancel-smi", "q"); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	waitIdle(t, "gw-cancel-smi")
	vendorQueries.Lock()
	q := vendorQueries.byKey["gw-cancel-smi\x00q"]
	out, okAt := q.okOut, q.okAt
	vendorQueries.Unlock()
	if out != "answer 1\n" || okAt.IsZero() {
		t.Fatalf("the cancelled caller's query must still finish for the next caller: out=%q at=%s", out, okAt)
	}
}

// Once a CLI is known to be slow, a caller with an earlier answer is served
// it at once instead of waiting out its time for a query that will not end
// in it; a quick CLI's caller still waits for the fresh answer.
func TestQueryVendorCLIServesASlowCLIsAnswerWithoutWaiting(t *testing.T) {
	queryTimings(t, 300*time.Millisecond, 10*time.Second, time.Minute)
	dir, _ := countingCLI(t, "gw-known-slow-smi")
	if _, err := QueryVendorCLI(t.Context(), "gw-known-slow-smi", "q"); err != nil {
		t.Fatal(err)
	}
	if VendorCLISlow("gw-known-slow-smi") {
		t.Fatal("a quick CLI counts as slow")
	}
	setDelay(t, dir, "0.6")
	if out, err := QueryVendorCLI(t.Context(), "gw-known-slow-smi", "q"); err != nil || out != "answer 1\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if !VendorCLISlow("gw-known-slow-smi") {
		t.Fatal("a query running past the wait must mark the CLI slow")
	}
	start := time.Now()
	if out, err := QueryVendorCLI(t.Context(), "gw-known-slow-smi", "q"); err != nil || out != "answer 1\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("caller of a known-slow CLI waited %s", took)
	}
	waitIdle(t, "gw-known-slow-smi")
	if !VendorCLISlow("gw-known-slow-smi") {
		t.Fatal("a finished slow query must keep the CLI slow")
	}
	setDelay(t, dir, "0")
	// The next query is quick; once it has finished the CLI is no longer slow.
	QueryVendorCLI(t.Context(), "gw-known-slow-smi", "q")
	waitIdle(t, "gw-known-slow-smi")
	if VendorCLISlow("gw-known-slow-smi") {
		t.Fatal("a quick query must clear the slow mark")
	}
}
