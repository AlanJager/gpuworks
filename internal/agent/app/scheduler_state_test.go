package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/agent/slurm"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A node whose cards Slurm allocates reports its Slurm name and, when Slurm
// will not run jobs there, the code and Slurm's own reason; a node that is not
// a guest of Slurm reports none of the three.
func TestSchedulerStateReportsSlurmsAnswerOnlyOnASlurmNode(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), recording.New(filepath.Join(t.TempDir(), "rec.jsonl")),
		filepath.Join(t.TempDir(), "models"), "docker", &gpu.SMIProber{}, nil, engine.WithNetworkInterfaceResolver(nil))
	var sys proto.SysInfo
	n.SchedulerState(context.Background(), &sys)
	if sys.SlurmNode != "" || sys.SlurmUnavailable != "" || sys.SlurmReason != "" || sys.KubeUnschedulable {
		t.Fatalf("a node outside Slurm reported %+v", sys)
	}

	answer := "NodeName=gpu-1 Gres=gpu:2 State=IDLE+DRAIN+DYNAMIC_NORM+INVALID_REG Partitions=gpu Reason=gres/gpu count reported lower than configured (0 < 2) [slurm@2026-11-16T09:13:21]\n"
	var askErr error
	run := func(context.Context, string, ...string) ([]byte, error) { return []byte(answer), askErr }
	n.occupancy.slurm.Store(slurm.New(slurm.Config{Node: "gpu-1"}, run, nil, "", slog.New(slog.DiscardHandler)))

	sys = proto.SysInfo{}
	n.SchedulerState(context.Background(), &sys)
	if sys.SlurmNode != "gpu-1" || sys.SlurmUnavailable != proto.SlurmNodeGRESMissing ||
		sys.SlurmReason != "gres/gpu count reported lower than configured (0 < 2)" {
		t.Fatalf("a drained Slurm node reported node=%q code=%q reason=%q", sys.SlurmNode, sys.SlurmUnavailable, sys.SlurmReason)
	}

	askErr = errors.New("slurm_load_node: Unable to contact slurm controller")
	sys = proto.SysInfo{}
	n.SchedulerState(context.Background(), &sys)
	if sys.SlurmNode != "gpu-1" || sys.SlurmUnavailable != proto.SlurmNodeGRESMissing {
		t.Fatalf("an unreachable controller changed the report: %+v", sys)
	}
}
