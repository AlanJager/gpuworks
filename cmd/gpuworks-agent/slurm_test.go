package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/slurm"
	"gpuworks.ai/gpuworks/internal/agentconf"
)

type slurmEnrollFake struct {
	mu       sync.Mutex
	requests []map[string]string
	status   int
	reply    string
}

func (f *slurmEnrollFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.requests = append(f.requests, body)
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.reply)
		return
	}
	_, _ = io.WriteString(w, `{"node_uuid":"uuid-1","node_token":"node-token","settings":{"sbatch_args":"-A ai","idle_return_seconds":120,"pending_timeout_seconds":900,"account_user":"svc"}}`)
}

func slurmConfigOutput(cluster string) slurm.Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "scontrol" {
			return nil, errors.New("unexpected " + name)
		}
		if cluster == "" {
			return nil, errors.New("scontrol: error: slurm_load_ctl_conf: Unable to contact slurm controller")
		}
		return []byte("ClusterName             = " + cluster + "\n"), nil
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSlurmAcquireTokenEnrollsAnAgentWithoutANodeToken(t *testing.T) {
	fake := &slurmEnrollFake{}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	prev := slurmRunner
	slurmRunner = slurmConfigOutput("demo")
	t.Cleanup(func() { slurmRunner = prev })
	state := t.TempDir()
	conf := agentconf.Config{Server: ts.URL, SlurmEnrollToken: "enroll", SlurmNode: "gpu-1"}
	cfg := agentapp.Config{}
	if err := slurmAcquireToken(quietLog(), &conf, &cfg, state); err != nil {
		t.Fatal(err)
	}
	if conf.Token != "node-token" || cfg.Slurm == nil || cfg.Slurm.Node != "gpu-1" ||
		cfg.Slurm.Settings != (slurm.Settings{SbatchArgs: "-A ai", IdleReturnSeconds: 120, PendingTimeoutSeconds: 900, AccountUser: "svc"}) {
		t.Fatalf("conf %+v cfg.Slurm %+v", conf, cfg.Slurm)
	}
	if got := fake.requests[0]; got["slurm_cluster_name"] != "demo" || got["node_name"] != "gpu-1" || got["token"] != "enroll" {
		t.Fatalf("request = %v", got)
	}
	if err := conf.Validate(); err != nil {
		t.Fatalf("the enrolled configuration does not validate: %v", err)
	}
	// A node the server forgot enrolls again through the same entry.
	if cfg.Reenroll == nil {
		t.Fatal("an enrolled agent cannot enroll again")
	}
	if token, err := cfg.Reenroll(context.Background()); err != nil || token != "node-token" || len(fake.requests) != 2 {
		t.Fatalf("Reenroll = %q, %v (%d requests)", token, err, len(fake.requests))
	}
	fake.requests = fake.requests[:1]
	if saved := slurm.ReadIdentity(state, "enroll"); saved.NodeToken != "node-token" {
		t.Fatalf("identity not saved: %+v", saved)
	}
	// Commands that only talk to the server take the saved token without asking.
	later := agentconf.Config{Server: ts.URL, SlurmEnrollToken: "enroll", SlurmNode: "gpu-1"}
	useSavedSlurmIdentity(&later, state)
	if later.Token != "node-token" || len(fake.requests) != 1 {
		t.Fatalf("saved identity not used: %+v (%d requests)", later, len(fake.requests))
	}
	// A token given is never replaced, and an agent with no enrollment is left alone.
	for _, c := range []agentconf.Config{{Server: ts.URL, Token: "mine", SlurmEnrollToken: "enroll", SlurmNode: "n"}, {Server: ts.URL, Token: "mine"}} {
		cfg := agentapp.Config{}
		if err := slurmAcquireToken(quietLog(), &c, &cfg, t.TempDir()); err != nil || c.Token != "mine" || cfg.Slurm != nil || len(fake.requests) != 1 {
			t.Fatalf("an agent with its own token: %+v %+v %v (%d requests)", c, cfg.Slurm, err, len(fake.requests))
		}
	}
}

// The agent starts before the server or Slurm answers (the health check
// installs it first): it waits and asks again; a node that enrolled before
// keeps its identity meanwhile, except after a refusal about its own.
func TestEnrollSlurmNodeRetriesUntilTheServerAnswers(t *testing.T) {
	prev := slurmEnrollRetryDelay
	slurmEnrollRetryDelay = time.Millisecond
	t.Cleanup(func() { slurmEnrollRetryDelay = prev })
	fake := &slurmEnrollFake{status: http.StatusBadGateway, reply: "bad gateway"}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	go func() {
		time.Sleep(30 * time.Millisecond)
		fake.mu.Lock()
		fake.status = 0
		fake.mu.Unlock()
	}()
	// Slurm answers too: the first calls fail like a controller that is not up.
	calls := 0
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls++
		if calls < 3 {
			return slurmConfigOutput("")(ctx, name, args...)
		}
		return slurmConfigOutput("demo")(ctx, name, args...)
	}
	id, err := enrollSlurmNode(context.Background(), quietLog(), run, ts.URL, "enroll", "gpu-1", t.TempDir())
	if err != nil || id.NodeToken != "node-token" || calls < 3 {
		t.Fatalf("enrollSlurmNode = %+v, %v after %d Slurm calls", id, err, calls)
	}
}

func TestEnrollSlurmNodeUsesTheSavedIdentityWhenNothingAnswersExceptAfterARefusal(t *testing.T) {
	prev := slurmEnrollRetryDelay
	slurmEnrollRetryDelay = time.Millisecond
	t.Cleanup(func() { slurmEnrollRetryDelay = prev })
	state := t.TempDir()
	fake := &slurmEnrollFake{}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	if _, err := enrollSlurmNode(context.Background(), quietLog(), slurmConfigOutput("demo"), ts.URL, "enroll", "gpu-1", state); err != nil {
		t.Fatal(err)
	}
	// The controller is down at boot: the saved identity is used at once.
	id, err := enrollSlurmNode(context.Background(), quietLog(), slurmConfigOutput(""), ts.URL, "enroll", "gpu-1", state)
	if err != nil || id.NodeToken != "node-token" {
		t.Fatalf("with Slurm unreachable = %+v, %v", id, err)
	}
	// The node is held by another entry: nothing excuses it, the agent keeps
	// asking (and does not start on the old identity) until its context ends.
	fake.mu.Lock()
	fake.status, fake.reply = http.StatusForbidden, `{"error":"x","code":"SLURM_ENROLL_REFUSED","params":{"reason":"node_taken","entry":"hpc-b"}}`
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if id, err := enrollSlurmNode(ctx, quietLog(), slurmConfigOutput("demo"), ts.URL, "enroll", "gpu-1", state); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held node enrolled: %+v, %v", id, err)
	}
	if len(fake.requests) < 3 {
		t.Fatalf("the agent did not keep asking: %d requests", len(fake.requests))
	}
}

func TestConfigureWritesAnEnrollmentWhenGivenTheSlurmNode(t *testing.T) {
	state := t.TempDir()
	var out io.Writer = io.Discard
	if err := configure([]string{"--state", state, "--server", "https://gw:8800", "--slurm-node", "gpu-7"}, strings.NewReader("enroll-token\n"), out); err != nil {
		t.Fatal(err)
	}
	conf, err := agentconf.Load(state)
	if err != nil || conf.SlurmEnrollToken != "enroll-token" || conf.SlurmNode != "gpu-7" || conf.Token != "" || conf.Server != "https://gw:8800" {
		t.Fatalf("conf = %+v, %v", conf, err)
	}
	// Without the node name the token is the node's own, as it always was.
	other := t.TempDir()
	if err := configure([]string{"--state", other, "--server", "https://gw:8800"}, strings.NewReader("node-token\n"), out); err != nil {
		t.Fatal(err)
	}
	if conf, err := agentconf.Load(other); err != nil || conf.Token != "node-token" || conf.SlurmEnrollToken != "" {
		t.Fatalf("plain conf = %+v, %v", conf, err)
	}
}

func TestLaunchResolvesTheSlurmEnrollmentFromFileAndEnvironment(t *testing.T) {
	state := t.TempDir()
	if err := agentconf.Write(state, agentconf.Config{Server: "https://gw:8800", SlurmEnrollToken: "from-file", SlurmNode: "gpu-1"}); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	l := newLaunchFlags(fs)
	if err := fs.Parse([]string{"--state", state}); err != nil {
		t.Fatal(err)
	}
	conf, _, fromFile, err := l.resolve(func(k string) string {
		if k == "GPUWORKS_SLURM_NODE" {
			return "gpu-9"
		}
		return ""
	})
	if err != nil || !fromFile || conf.SlurmEnrollToken != "from-file" || conf.SlurmNode != "gpu-9" || conf.Token != "" {
		t.Fatalf("resolved = %+v (file %v), %v", conf, fromFile, err)
	}
}
