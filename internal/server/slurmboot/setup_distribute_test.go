package slurmboot

import (
	"strings"
	"testing"
)

// maxOverlap is the most nodes that had a command in flight at once, from the
// fake ssh's start/end events (a node's span runs from its first event to its
// last).
func (w *world) maxOverlap() int {
	first, last := map[string]int{}, map[string]int{}
	for i, line := range strings.Split(strings.TrimSpace(w.stateFile("ssh-events")), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if _, ok := first[f[1]]; !ok {
			first[f[1]] = i
		}
		last[f[1]] = i
	}
	best := 0
	for n := range first {
		overlap := 0
		for m := range first {
			if first[m] <= first[n] && first[n] <= last[m] {
				overlap++
			}
		}
		best = max(best, overlap)
	}
	return best
}

func (w *world) manyNodes(n int) {
	w.t.Helper()
	var names []string
	for i := 0; i < n; i++ {
		names = append(names, "cn"+string(rune('a'+i)))
	}
	w.write(w.state+"/sinfo", strings.Join(names, "\n")+"\nctl\n")
}

// The nodes are copied to in parallel, a bounded number at a time, instead of
// one after the other.
func TestSetupCopiesToNodesInParallelWithABound(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.onlyRemoteTools("ssh", "scp")
		w.manyNodes(6)
		w.env["FAKE_SSH_SLEEP"] = "0.2"
		w.env["GWKS_DIST_JOBS"] = "3"
		r := w.runSetup()
		if r.code != 0 || marker(t, r.stdout, "distribute_summary")["ok"] != "6" {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		// The overlap is the whole claim: more than one node at a time, never
		// more than the bound. How long that takes depends on the machine's
		// load, so no wall-clock limit is asserted.
		if got := w.maxOverlap(); got != 3 {
			t.Fatalf("%d nodes at once, want exactly the bound of 3", got)
		}
	})
}

// A node not started within the time budget is not left silently: it is
// listed as failed, with why, and the rest of the run goes on.
func TestSetupStopsStartingNodesAfterTheTimeBudget(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.onlyRemoteTools("ssh", "scp")
		w.manyNodes(3)
		w.env["FAKE_SSH_SLEEP"] = "0.5"
		w.env["GWKS_DIST_JOBS"] = "1"
		w.env["GWKS_DIST_BUDGET"] = "1"
		r := w.runSetup()
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		byNode := map[string]map[string]string{}
		for _, m := range markers(t, r.stdout, "distribute") {
			byNode[m["node"]] = m
		}
		if byNode["cna"]["status"] != "ok" || byNode["cnb"]["reason"] != "time_cap" || byNode["cnc"]["reason"] != "time_cap" {
			t.Fatalf("per node = %v", byNode)
		}
		if s := marker(t, r.stdout, "distribute_summary"); s["ok"] != "1" || s["failed"] != "cnb,cnc" {
			t.Fatalf("summary = %v", s)
		}
		if marker(t, r.stdout, "done")["status"] != "partial" || !strings.Contains(r.stdout, "cnb: FAILED (time_cap)") {
			t.Fatalf("output:\n%s", r.stdout)
		}
	})
}

// Each node says why it failed: not reachable, or reachable but not writable.
func TestSetupSaysWhyANodeFailed(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.onlyRemoteTools("ssh", "scp")
		w.env["FAKE_FAIL_NODES"] = "cn001"
		w.env["FAKE_SCP_FAIL_NODES"] = "cn002"
		r := w.runSetup()
		reasons := map[string]string{}
		for _, m := range markers(t, r.stdout, "distribute") {
			reasons[m["node"]] = m["status"] + "/" + m["reason"]
		}
		if reasons["cn000"] != "ok/" || reasons["cn001"] != "failed/unreachable" || reasons["cn002"] != "failed/copy" {
			t.Fatalf("reasons = %v", reasons)
		}
	})
}

// A script directory that is not shared storage gets the warning that nodes
// added later need the files, on every run and in the output a person reads.
func TestSetupWarnsThatLaterNodesNeedTheFiles(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.onlyRemoteTools("ssh", "scp")
		r := w.runSetup()
		if !strings.Contains(r.stdout, "a node added to the cluster later has none of these files") || !strings.Contains(r.stdout, "Run this command again after adding nodes") {
			t.Fatalf("no warning:\n%s", r.stdout)
		}
		if marker(t, r.stdout, "done")["status"] != "ok" {
			t.Fatal("the notice makes a complete copy partial")
		}
	})
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		if r := w.runSetup(); strings.Contains(r.stdout, "added to the cluster later") {
			t.Fatalf("a shared directory warned:\n%s", r.stdout)
		}
	})
}
