package app

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/slurm"
)

func TestLeaserConfigTakesAnEnrolledNodesSettingsFromItsEntry(t *testing.T) {
	env := map[string]string{"GPUWORKS_SLURM_SBATCH_ARGS": "-A env", "GPUWORKS_SLURM_PENDING_TIMEOUT": "1m", "GPUWORKS_SLURM_IDLE_RETURN": "5s"}
	getenv := func(k string) string { return env[k] }

	// A node that is not enrolled reads the environment, as it always did.
	plain := &slurmNode{getenv: getenv}
	got := plain.leaserConfig("gpu-1")
	if got.Node != "gpu-1" || !slices.Equal(got.SbatchArgs, []string{"-A", "env"}) || got.PendingTimeout != time.Minute || got.IdleReturn != 5*time.Second || got.AccountUser != "" || got.OrderFile != "" {
		t.Fatalf("plain config = %+v", got)
	}

	// An enrolled one reads its entry, and the environment does not apply.
	state := t.TempDir()
	enrolled := &slurmNode{getenv: getenv, enrolled: &SlurmEnrollment{Node: "gpu-1", StateDir: state}}
	settings := slurm.Settings{SbatchArgs: "-A ai --time=7-0", IdleReturnSeconds: 120, PendingTimeoutSeconds: 900, AccountUser: "svc"}
	enrolled.latest.Store(&settings)
	got = enrolled.leaserConfig("gpu-1")
	if !slices.Equal(got.SbatchArgs, []string{"-A", "ai", "--time=7-0"}) || got.PendingTimeout != 15*time.Minute || got.IdleReturn != 2*time.Minute ||
		got.AccountUser != "svc" || got.OrderFile != filepath.Join(state, "slurm-gres-devices") || got.SubmittersFile != filepath.Join(state, "slurm-submitters") {
		t.Fatalf("enrolled config = %+v", got)
	}
	// What a report said later is what the next leaser starts with.
	later := slurm.Settings{SbatchArgs: "-p gpu", IdleReturnSeconds: 30, PendingTimeoutSeconds: 60}
	enrolled.latest.Store(&later)
	if got := enrolled.leaserConfig("gpu-1"); !slices.Equal(got.SbatchArgs, []string{"-p", "gpu"}) || got.IdleReturn != 30*time.Second || got.AccountUser != "" {
		t.Fatalf("config after a report = %+v", got)
	}
}

// The reporter must not keep the token the agent started with: after the
// agent registers (the server issues another) or enrolls again, it reports
// with the one the agent holds now, or the server answers 401 forever.
func TestSlurmEnrollmentTokenFollowsTheNodesToken(t *testing.T) {
	n := &NodeRuntime{}
	n.SetNodeToken("first")
	e := bindSlurmEnrollment(SlurmEnrollment{Node: "gpu-1", Settings: slurm.Settings{SbatchArgs: "-A ai"}}, "http://gw:8800", n.currentNodeToken, "/state")
	if e.Server != "http://gw:8800" || e.StateDir != "/state" || e.Node != "gpu-1" || e.Settings.SbatchArgs != "-A ai" || e.Token() != "first" {
		t.Fatalf("enrollment = %+v", e)
	}
	n.SetNodeToken("issued-at-registration")
	if got := e.Token(); got != "issued-at-registration" {
		t.Fatalf("token after the server issued another = %q", got)
	}
	n.SetNodeToken("after-reenroll")
	if got := e.Token(); got != "after-reenroll" {
		t.Fatalf("token after enrolling again = %q", got)
	}
}
