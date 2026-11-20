package slurm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ehpcLine is `scontrol -o show node gpu000` of a fresh Alibaba Cloud E-HPC
// cluster (Slurm 22.05.8, dynamic node, no gres.conf), verbatim: double
// spaces, an OS= value that holds spaces, Gres still naming the GPU, and the
// Reason last with Slurm's stamp.
const ehpcLine = "NodeName=gpu000 Arch=x86_64 CoresPerSocket=8  CPUAlloc=0 CPUEfctv=16 CPUTot=16 CPULoad=0.74 AvailableFeatures=gpuFeature ActiveFeatures=gpuFeature Gres=gpu:1 NodeAddr=172.30.5.40 NodeHostName=gpu000 Version=22.05.8 OS=Linux 5.15.0-177-generic #187-Ubuntu SMP Sat Apr 11 22:54:33 UTC 2026  RealMemory=61898 AllocMem=0 FreeMem=58525 Sockets=1 Boards=1 State=IDLE+DRAIN+DYNAMIC_NORM+INVALID_REG ThreadsPerCore=2 TmpDisk=0 Weight=1 Owner=N/A MCS_label=N/A Partitions=gpu  BootTime=2026-11-16T23:39:16 SlurmdStartTime=2026-11-16T23:41:26 LastBusyTime=2026-11-16T23:41:26 CfgTRES=cpu=16,mem=61898M,billing=16 AllocTRES= CapWatts=n/a CurrentWatts=0 AveWatts=0 ExtSensorsJoules=n/s ExtSensorsWatts=0 ExtSensorsTemp=n/s Reason=gres/gpu count reported lower than configured (0 < 1) [root@2026-11-16T23:41:26]"

// resumedLine is the same node after `scontrol update state=resume`: State
// without DRAIN and INVALID_REG, and no Reason field at all.
var resumedLine = func() string {
	line := strings.Replace(ehpcLine, "State=IDLE+DRAIN+DYNAMIC_NORM+INVALID_REG", "State=IDLE+DYNAMIC_NORM", 1)
	return line[:strings.Index(line, " Reason=")]
}()

func TestParseNodeState(t *testing.T) {
	line := func(state, reason string) string {
		l := "NodeName=n1 Gres=gpu:2 State=" + state + " Partitions=gpu BootTime=2026-11-16T09:12:01"
		if reason != "" {
			l += " Reason=" + reason
		}
		return l
	}
	for _, tc := range []struct {
		name       string
		line       string
		wantCode   string
		wantReason string
	}{
		{"gres missing, the E-HPC line", ehpcLine, proto.SlurmNodeGRESMissing, "gres/gpu count reported lower than configured (0 < 1)"},
		// gres.conf fixed and slurmd restarted, not yet resumed: INVALID_REG is
		// gone while the old Reason stays, so only the resume is left to do.
		{"fixed but not yet resumed", strings.Replace(ehpcLine, "+INVALID_REG", "", 1),
			proto.SlurmNodeDrained, "gres/gpu count reported lower than configured (0 < 1)"},
		{"resumed, no Reason field at all", resumedLine, "", ""},
		{"gres missing without the stamp", line("IDLE+DRAIN+INVALID_REG", "gres/gpu count reported lower than configured (0 < 1)"),
			proto.SlurmNodeGRESMissing, "gres/gpu count reported lower than configured (0 < 1)"},
		{"another refused registration", line("IDLE+DRAIN+INVALID_REG", "Low RealMemory [slurm@2026-11-16T09:13:21]"),
			proto.SlurmNodeInvalidRegistration, "Low RealMemory"},
		{"invalid registration without a reason", line("DOWN+INVALID_REG", ""), proto.SlurmNodeInvalidRegistration, ""},
		{"down and not responding", line("DOWN+DRAIN+NOT_RESPONDING", "Not responding [slurm@2026-11-16T09:30:00]"),
			proto.SlurmNodeDown, "Not responding"},
		{"base state down", line("DOWN", "Kill task failed [root@2026-11-16T09:30:00]"), proto.SlurmNodeDown, "Kill task failed"},
		{"base state down with the star sinfo prints", line("DOWN*", ""), proto.SlurmNodeDown, ""},
		{"not responding alone", line("IDLE+NOT_RESPONDING", ""), proto.SlurmNodeDown, ""},
		{"failing", line("MIXED+DRAIN+FAIL", "fail [root@2026-11-16T09:30:00]"), proto.SlurmNodeDown, "fail"},
		{"drained by an operator", line("IDLE+DRAIN", "maintenance window [root@2026-11-16T09:30:00]"),
			proto.SlurmNodeDrained, "maintenance window"},
		{"draining with jobs still running", line("ALLOCATED+DRAIN", "reboot after jobs [root@2026-11-16T09:30:00]"),
			proto.SlurmNodeDrained, "reboot after jobs"},
		{"a reason that holds a bracket and an equals sign", line("IDLE+DRAIN", "ticket [4711] a=b [root@2026-11-16T09:30:00]"),
			proto.SlurmNodeDrained, "ticket [4711] a=b"},
		{"the reason has other fields after it", line("IDLE+DRAIN", "x y [root@2026-11-16T09:30:00] Comment=(null)"), proto.SlurmNodeDrained, "x y"},
		{"idle", line("IDLE", ""), "", ""},
		{"idle with an old reason", line("IDLE", "was drained [root@2026-11-16T09:30:00]"), "", ""},
		{"mixed", line("MIXED", ""), "", ""},
		{"allocated and dynamic", line("ALLOCATED+DYNAMIC_NORM", ""), "", ""},
		{"null reason on a drained node", line("IDLE+DRAIN", "(null)"), proto.SlurmNodeDrained, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNodeState(tc.line)
			if got.code != tc.wantCode || got.reason != tc.wantReason {
				t.Fatalf("parseNodeState = {%q, %q}; want {%q, %q}", got.code, got.reason, tc.wantCode, tc.wantReason)
			}
		})
	}
}

// The real line, through the leaser's own scontrol call, for the node it names.
func TestNodeStateReadsARealEHPCLine(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "scontrol" || len(args) != 4 || args[3] != "gpu000" {
			return nil, fmt.Errorf("unexpected call %s %v", name, args)
		}
		return []byte(ehpcLine + "\n"), nil
	}
	l := New(Config{Node: "gpu000"}, run, nil, "", nil)
	node, code, reason := l.NodeState(context.Background())
	if node != "gpu000" || code != proto.SlurmNodeGRESMissing || reason != "gres/gpu count reported lower than configured (0 < 1)" {
		t.Fatalf("NodeState = %q, %q, %q", node, code, reason)
	}
}

func TestNodeStateReportsWhatSlurmSays(t *testing.T) {
	f := newFakeSlurm()
	l, _ := newTestLeaser(t, f, Config{})
	ctx := context.Background()
	node, code, reason := l.NodeState(ctx)
	if node != f.node || code != "" || reason != "" {
		t.Fatalf("a node that runs jobs: NodeState = %q, %q, %q; want its name only", node, code, reason)
	}
	f.state, f.reason = "IDLE+DRAIN+DYNAMIC_NORM+INVALID_REG", "gres/gpu count reported lower than configured (0 < 2) [slurm@2026-11-16T09:13:21]"
	if node, code, reason = l.NodeState(ctx); node != f.node || code != proto.SlurmNodeGRESMissing || reason != "gres/gpu count reported lower than configured (0 < 2)" {
		t.Fatalf("NodeState = %q, %q, %q; want gres_missing with Slurm's reason", node, code, reason)
	}
	f.state, f.reason = "IDLE", ""
	if _, code, reason = l.NodeState(ctx); code != "" || reason != "" {
		t.Fatalf("after resume: NodeState code=%q reason=%q; want both empty", code, reason)
	}
}

// A controller that does not answer is no news: the last answer stands, so a
// node is neither cordoned nor released by one failed call.
func TestNodeStateRestatesTheLastAnswerWhenScontrolFails(t *testing.T) {
	f := newFakeSlurm()
	f.state, f.reason = "IDLE+DRAIN", "maintenance [root@2026-11-16T09:30:00]"
	l, _ := newTestLeaser(t, f, Config{})
	ctx := context.Background()
	if _, code, _ := l.NodeState(ctx); code != proto.SlurmNodeDrained {
		t.Fatalf("code = %q; want drained", code)
	}
	f.mu.Lock()
	f.failCmd = "scontrol -o show node"
	f.mu.Unlock()
	if _, code, reason := l.NodeState(ctx); code != proto.SlurmNodeDrained || reason != "maintenance" {
		t.Fatalf("scontrol failing: code=%q reason=%q; want the last answer restated", code, reason)
	}
	// Once it answers again, so does the leaser.
	f.mu.Lock()
	f.failCmd, f.state, f.reason = "", "IDLE", ""
	f.mu.Unlock()
	if _, code, _ := l.NodeState(ctx); code != "" {
		t.Fatalf("scontrol answering again: code=%q; want none", code)
	}
}

// Before the first answer there is nothing to restate: the node is reported
// as Slurm able to run jobs, which cordons nothing.
func TestNodeStateWithNoAnswerYetReportsNothingWrong(t *testing.T) {
	f := newFakeSlurm()
	f.failCmd = "scontrol -o show node"
	l, _ := newTestLeaser(t, f, Config{})
	if node, code, reason := l.NodeState(context.Background()); node != f.node || code != "" || reason != "" {
		t.Fatalf("NodeState = %q, %q, %q", node, code, reason)
	}
}

// A different node's record is not this node's state.
func TestNodeStateIgnoresARecordOfAnotherNode(t *testing.T) {
	var l *Leaser
	run := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("NodeName=other State=DOWN Reason=x\n"), nil
	}
	l = New(Config{Node: "mine"}, run, nil, "", nil)
	if _, code, _ := l.NodeState(context.Background()); code != "" {
		t.Fatalf("code = %q; want none for another node's record", code)
	}
}
