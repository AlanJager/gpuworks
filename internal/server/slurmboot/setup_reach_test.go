package slurmboot

import (
	"path/filepath"
	"strings"
	"testing"
)

// gpuNodes makes Slurm list these nodes with a gpu Gres and cpu ones without,
// and the controller itself (which the setup never asks over ssh).
func (w *world) gpuNodes(gpu []string, cpu ...string) {
	w.t.Helper()
	var lines []string
	for _, n := range gpu {
		lines = append(lines, "NodeName="+n+" Arch=x86_64 CPUTot=16 Gres=gpu:a100:2(S:0-1) NodeAddr="+n+" State=IDLE Partitions=gpu")
	}
	for _, n := range cpu {
		lines = append(lines, "NodeName="+n+" Arch=x86_64 CPUTot=16 Gres=(null) NodeAddr="+n+" State=IDLE Partitions=cpu")
	}
	lines = append(lines, "NodeName=ctl Arch=x86_64 CPUTot=4 Gres=gpu:1 NodeAddr=ctl State=IDLE Partitions=gpu")
	w.write(filepath.Join(w.state, "node-list"), strings.Join(lines, "\n")+"\n")
}

// reach reads what a run said about every host: "role/host" to "status/reason".
func reach(t *testing.T, out string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, m := range markers(t, out, "server_reach") {
		got[m["role"]+"/"+m["host"]] = m["status"] + "/" + m["reason"]
	}
	return got
}

func (w *world) healthzAsked() []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(w.stateFile("healthz")), "\n") {
		if f := strings.Fields(l); len(f) == 3 {
			out = append(out, f[1])
		}
	}
	return out
}

// Every host that can reach the server is said to, once, and the run is not
// partial because of it: the controller and each GPU node (not the CPU nodes).
func TestSetupAsksTheControllerAndEveryGPUNodeForTheServer(t *testing.T) {
	for _, tool := range []string{"ssh", "clush", "pdsh"} {
		eachShell(t, func(t *testing.T, w *world) {
			w.controller("nfs4", baseConf)
			w.gpuNodes([]string{"gpu000", "gpu001"}, "cpu000")
			switch tool {
			case "ssh":
				w.onlyRemoteTools("ssh", "scp")
			case "clush":
				w.onlyRemoteTools("clush")
			default:
				w.onlyRemoteTools("pdsh", "pdcp")
			}
			r := w.runSetup()
			if r.code != 0 {
				t.Fatalf("%s: exit %d\n%s%s", tool, r.code, r.stdout, r.stderr)
			}
			if got := reach(t, r.stdout); len(got) != 3 || got["controller/ctl"] != "ok/" || got["node/gpu000"] != "ok/" || got["node/gpu001"] != "ok/" {
				t.Fatalf("%s: reach = %v", tool, got)
			}
			if s := marker(t, r.stdout, "server_reach_summary"); s["ok"] != "3" || s["failed"] != "none" || s["unknown"] != "none" || s["url"] != testServer {
				t.Fatalf("%s: summary = %v", tool, s)
			}
			if marker(t, r.stdout, "done")["status"] != "ok" {
				t.Fatalf("%s: done = %v", tool, marker(t, r.stdout, "done"))
			}
			asked := strings.Join(w.healthzAsked(), ",")
			if !strings.Contains(asked, "ctl") || !strings.Contains(asked, "gpu000") || !strings.Contains(asked, "gpu001") || strings.Contains(asked, "cpu000") {
				t.Fatalf("%s: curl ran on %s", tool, asked)
			}
		})
	}
}

// A host that cannot reach the server makes the run partial and the reason is
// the one curl gave, per host, as a word.
func TestSetupSaysWhichHostCannotReachTheServerAndWhy(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		w.gpuNodes([]string{"gpu000", "gpu001", "gpu002", "gpu003", "gpu004", "gpu005", "gpu006", "gpu007"})
		w.onlyRemoteTools("ssh", "scp")
		w.env["FAKE_HEALTHZ"] = "ctl=7 gpu000=6 gpu001=7 gpu002=28 gpu003=60 gpu004=22 gpu005=1 gpu006=127"
		r := w.runSetup()
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		got := reach(t, r.stdout)
		for host, want := range map[string]string{
			"controller/ctl": "failed/connect", "node/gpu000": "failed/dns", "node/gpu001": "failed/connect", "node/gpu002": "failed/timeout",
			"node/gpu003": "failed/tls", "node/gpu004": "failed/http", "node/gpu005": "failed/other", "node/gpu006": "failed/no_curl", "node/gpu007": "ok/",
		} {
			if got[host] != want {
				t.Fatalf("%s = %q, want %q (all: %v)", host, got[host], want, got)
			}
		}
		s := marker(t, r.stdout, "server_reach_summary")
		if s["ok"] != "1" || s["failed"] != "ctl,gpu000,gpu001,gpu002,gpu003,gpu004,gpu005,gpu006" {
			t.Fatalf("summary = %v", s)
		}
		if marker(t, r.stdout, "done")["status"] != "partial" {
			t.Fatalf("done = %v", marker(t, r.stdout, "done"))
		}
		if !strings.Contains(r.stdout, "gpu002: CANNOT reach "+testServer) || !strings.Contains(r.stdout, "cannot reach "+testServer+": ") {
			t.Fatalf("output does not say which host:\n%s", r.stdout)
		}
		// Not reaching the server does not stop the install: the cluster is set up.
		if !strings.Contains(w.read(w.confPath()), "HealthCheckProgram=") {
			t.Fatal("slurm.conf was not changed")
		}
	})
}

// A node nothing can run a command on is said not to have been asked; that is
// no failure of the server's address, so the run is not partial because of it.
func TestSetupSaysWhichNodesItCouldNotAsk(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		w.gpuNodes([]string{"gpu000", "gpu001"})
		w.onlyRemoteTools("ssh", "scp")
		w.env["FAKE_FAIL_NODES"] = "gpu001"
		r := w.runSetup()
		got := reach(t, r.stdout)
		if got["node/gpu000"] != "ok/" || got["node/gpu001"] != "unknown/not_asked" {
			t.Fatalf("reach = %v", got)
		}
		if s := marker(t, r.stdout, "server_reach_summary"); s["unknown"] != "gpu001" || s["failed"] != "none" {
			t.Fatalf("summary = %v", s)
		}
		if marker(t, r.stdout, "done")["status"] != "ok" {
			t.Fatalf("done = %v", marker(t, r.stdout, "done"))
		}
	})
}

// --no-node-reach asks the controller only; a node is not touched.
func TestSetupCanAskTheControllerOnly(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		w.gpuNodes([]string{"gpu000"})
		w.onlyRemoteTools("ssh", "scp")
		r := w.runSetup("--no-node-reach")
		if got := reach(t, r.stdout); len(got) != 1 || got["controller/ctl"] != "ok/" {
			t.Fatalf("reach = %v", got)
		}
		if w.calls("ssh") != 0 {
			t.Fatalf("ssh was run:\n%s", w.stateFile("calls"))
		}
	})
}

// The nodes are asked a bounded number at a time, and within the time budget:
// one not started by then is said not to have been asked.
func TestSetupAsksNodesInParallelWithABound(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		w.gpuNodes([]string{"g0", "g1", "g2", "g3", "g4", "g5"})
		w.onlyRemoteTools("ssh", "scp")
		w.env["FAKE_SSH_SLEEP"] = "0.2"
		w.env["GWKS_DIST_JOBS"] = "2"
		r := w.runSetup()
		if got := w.maxOverlap(); got != 2 {
			t.Fatalf("%d nodes at once, want the bound of 2", got)
		}
		if s := marker(t, r.stdout, "server_reach_summary"); s["ok"] != "7" {
			t.Fatalf("summary = %v", s)
		}
	})
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs4", baseConf)
		w.gpuNodes([]string{"g0", "g1", "g2"})
		w.onlyRemoteTools("ssh", "scp")
		// one node takes longer than the whole budget, so none starts after it
		w.env["FAKE_SSH_SLEEP"] = "1.2"
		w.env["GWKS_DIST_JOBS"] = "1"
		w.env["GWKS_DIST_BUDGET"] = "1"
		r := w.runSetup()
		got := reach(t, r.stdout)
		if got["node/g0"] != "ok/" || got["node/g1"] != "unknown/not_asked" || got["node/g2"] != "unknown/not_asked" {
			t.Fatalf("reach = %v", got)
		}
	})
}

// --check asks the same questions with nothing written and nothing copied: the
// only commands a node is given are the curl, and the run says partial when a
// host cannot reach the server.
func TestCheckAsksForTheServerAndChangesNothing(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("ext4", baseConf)
		w.gpuNodes([]string{"gpu000", "gpu001"})
		w.onlyRemoteTools("ssh", "scp")
		w.env["FAKE_HEALTHZ"] = "gpu001=28"
		before := w.snapshot()
		r := w.runSetup("--check")
		if r.code != 0 {
			t.Fatalf("exit %d\n%s", r.code, r.stdout)
		}
		if d := diffSnapshots(before, w.snapshot()); len(d) != 0 {
			t.Fatalf("--check changed the machine: %v", d)
		}
		got := reach(t, r.stdout)
		if got["controller/ctl"] != "ok/" || got["node/gpu000"] != "ok/" || got["node/gpu001"] != "failed/timeout" {
			t.Fatalf("reach = %v", got)
		}
		if w.calls("scp") != 0 {
			t.Fatalf("--check copied:\n%s", w.stateFile("calls"))
		}
		for _, l := range strings.Split(w.stateFile("calls"), "\n") {
			if strings.HasPrefix(l, "ssh ") && !strings.Contains(l, "GWKS_REACH_RC") {
				t.Fatalf("--check ran something else on a node: %s", l)
			}
		}
		if m := marker(t, r.stdout, "done"); m["status"] != "partial" || m["mode"] != "check" {
			t.Fatalf("done = %v", m)
		}
	})
}
