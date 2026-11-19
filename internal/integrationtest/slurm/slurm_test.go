// Package slurm checks the exchange between a Slurm node's agent and the
// server end to end: the agent's own enrollment and report code against the
// real server, through the shapes both take from proto.
package slurm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	agentslurm "gpuworks.ai/gpuworks/internal/agent/slurm"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// The agent falls back to the entry's own defaults when an answer gives no
// duration, so an entry left at its defaults and an agent told nothing behave
// alike. The report interval is one constant both sides take from proto.
func TestTheAgentsDefaultsAreTheEntrysDefaults(t *testing.T) {
	if agentslurm.DefaultIdleReturn != store.SlurmDefaultIdleReturnSeconds*time.Second {
		t.Errorf("agent idle return %v, entry default %d s", agentslurm.DefaultIdleReturn, store.SlurmDefaultIdleReturnSeconds)
	}
	if agentslurm.DefaultPendingTimeout != store.SlurmDefaultPendingTimeoutSeconds*time.Second {
		t.Errorf("agent pending timeout %v, entry default %d s", agentslurm.DefaultPendingTimeout, store.SlurmDefaultPendingTimeoutSeconds)
	}
	if agentslurm.ReportInterval != proto.SlurmReportIntervalSeconds*time.Second {
		t.Errorf("agent report interval %v, proto %d s", agentslurm.ReportInterval, proto.SlurmReportIntervalSeconds)
	}
}

// sinfo prints a fixed cluster: two GPU nodes and one without GPUs.
func sinfo(out *string, mu *sync.Mutex) agentslurm.Runner {
	return func(_ context.Context, name string, _ ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if name != "sinfo" {
			return nil, nil
		}
		return []byte(*out), nil
	}
}

// A node's agent enrolls with the entry's token and gets its node and the
// entry's settings; its report stores the cluster's GPU nodes; the same list
// again is not stored again; an edit of the entry's settings reaches the
// agent with the next answer, while a change of the list right after a write
// waits; enrolling again is the same node; another Slurm cluster is refused.
func TestASlurmNodeEnrollsAndReportsThroughTheRealServer(t *testing.T) {
	env := testenv.New(t)
	entry, err := env.Store().CreateSlurmCluster(store.SlurmCluster{
		Name: "lab", ServerURL: env.URL(), RunAs: store.SlurmDefaultRunAs,
		InstallDir: store.SlurmDefaultInstallDir, ScriptDir: store.SlurmDefaultScriptDir,
		SbatchArgs: "-A ai", IdleReturnSeconds: 120, PendingTimeoutSeconds: 900, AccountUser: "svc",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	id, err := agentslurm.Enroll(ctx, nil, env.URL(), entry.Token, "labcluster", "gpu0", t.TempDir(), false)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	want := agentslurm.Settings{SbatchArgs: "-A ai", IdleReturnSeconds: 120, PendingTimeoutSeconds: 900, AccountUser: "svc"}
	if id.NodeUUID == "" || id.NodeToken == "" || id.Settings != want {
		t.Fatalf("identity = %+v", id)
	}
	node, err := env.Store().NodeByUUID(id.NodeUUID)
	if err != nil || node.SlurmNode != "gpu0" || node.SlurmClusterID != entry.ID {
		t.Fatalf("enrolled node = %+v, %v", node, err)
	}

	var mu sync.Mutex
	cluster := "gpu0|idle|gpu:2|none\ngpu1|mixed|gpu:a100:1(S:0)|\ncpu0|idle|(null)|\n"
	var applied []agentslurm.Settings
	r := &agentslurm.Reporter{
		Server: env.URL(), Token: func() string { return id.NodeToken }, Command: sinfo(&cluster, &mu),
		Apply: func(s agentslurm.Settings) { applied = append(applied, s) },
	}
	if err := r.Once(ctx); err != nil {
		t.Fatalf("first report: %v", err)
	}
	names, err := env.Store().SlurmGPUNodeNames(entry.ID)
	if err != nil || !reflect.DeepEqual(names, []string{"gpu0", "gpu1"}) {
		t.Fatalf("stored GPU nodes = %v, %v", names, err)
	}
	if len(applied) != 1 || applied[0] != want {
		t.Fatalf("settings applied = %+v", applied)
	}

	// The same cluster again: the server already has it and stores nothing.
	before, _ := env.Store().SlurmClusterByID(entry.ID)
	if err := r.Once(ctx); err != nil {
		t.Fatalf("second report: %v", err)
	}
	after, _ := env.Store().SlurmClusterByID(entry.ID)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("an unchanged report changed the entry:\n%+v\n%+v", before, after)
	}

	// A node gone from Slurm, and an edit of the settings. The list was
	// written a moment ago, so its change waits for a later report (one write
	// per entry per minute, which the console's freshness counts on); the
	// edit reaches the agent with this answer all the same.
	mu.Lock()
	cluster = strings.Replace(cluster, "gpu1|mixed|gpu:a100:1(S:0)|\n", "", 1)
	mu.Unlock()
	entry.SbatchArgs, entry.IdleReturnSeconds, entry.PendingTimeoutSeconds = "-p gpu", 30, 60
	if _, err := env.Store().UpdateSlurmCluster(*entry); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatalf("third report: %v", err)
	}
	names, err = env.Store().SlurmGPUNodeNames(entry.ID)
	if err != nil || !reflect.DeepEqual(names, []string{"gpu0", "gpu1"}) {
		t.Fatalf("stored GPU nodes right after a write = %v, %v; want the change to wait", names, err)
	}
	edited := agentslurm.Settings{SbatchArgs: "-p gpu", IdleReturnSeconds: 30, PendingTimeoutSeconds: 60, AccountUser: "svc"}
	if got := applied[len(applied)-1]; got != edited {
		t.Fatalf("settings after the edit = %+v, want %+v", got, edited)
	}

	// Enrolling again gives the same node, with the edited settings.
	again, err := agentslurm.Enroll(ctx, nil, env.URL(), entry.Token, "labcluster", "gpu0", t.TempDir(), false)
	if err != nil || again.NodeUUID != id.NodeUUID || again.Settings != edited {
		t.Fatalf("enroll again = %+v, %v", again, err)
	}
	// Another Slurm cluster cannot use this entry's token.
	_, err = agentslurm.Enroll(ctx, nil, env.URL(), entry.Token, "othercluster", "gpu9", t.TempDir(), false)
	var refusal *agentslurm.EnrollRefusal
	if !errors.As(err, &refusal) || refusal.Reason != proto.SlurmEnrollOtherCluster {
		t.Fatalf("enroll from another cluster = %v", err)
	}
}
