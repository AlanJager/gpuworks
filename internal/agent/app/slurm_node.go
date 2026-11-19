package app

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/slurm"
	"gpuworks.ai/gpuworks/internal/proto"
)

// What a Slurm compute node adds to an agent (ADR 0075): Slurm is the one
// allocator of the node's cards. Other jobs' cards are reported as held
// outside the platform, and an instance's cards are borrowed with a
// placeholder job before it starts and returned once no instance uses them.
//
// An agent a Slurm cluster entry's health check installed (SlurmEnrollment)
// takes its node name and these settings from the entry, which the server
// hands it at enrollment and on every report (slurm.Reporter), and the
// environment below does not apply. On any other node the environment tunes
// it; nothing is needed on a node whose host name is its Slurm node name:
//
//	GPUWORKS_SLURM                  auto (default) | on | off
//	GPUWORKS_SLURM_NODE             Slurm node name (default: short host name)
//	GPUWORKS_SLURM_SBATCH_ARGS      extra placeholder job options, e.g. "-A inference --time=7-0"
//	GPUWORKS_SLURM_PENDING_TIMEOUT  how long a placeholder job may wait to start (default 10m)
//	GPUWORKS_SLURM_IDLE_RETURN      how long an unused card is kept (default 60s)

// slurmSweepInterval is how often the leaser reads the node's jobs: it
// returns idle cards and notices placeholder jobs ended from outside.
const slurmSweepInterval = 15 * time.Second

// SlurmOrderFile is the file in the state directory where the health check
// leaves `slurmd -G` for an agent that is not root, which cannot run it on a
// configless node (slurm.Config.OrderFile).
const SlurmOrderFile = "slurm-gres-devices"

// SlurmSubmittersFile is the file in the state directory where the leaser
// keeps the accounts it submitted placeholder jobs as (slurm.Config.SubmittersFile).
const SlurmSubmittersFile = "slurm-submitters"

// SlurmEnrollment is what an agent enrolled through a Slurm cluster entry
// knows of it: this node's Slurm name and the entry's settings from
// enrollment, and where to report the cluster's GPU nodes (Server and Token,
// which are the agent's own once it has registered).
type SlurmEnrollment struct {
	Node     string
	Settings slurm.Settings
	// Server is the server's address; Token gives this node's token as it is
	// now (it changes when the agent registers or enrolls again).
	Server string
	Token  func() string
	// StateDir is the agent's state directory.
	StateDir string
}

// bindSlurmEnrollment is the enrollment the agent runs with: what the entry
// gave at enrollment, with the server, the state directory and the node's token
// as the agent holds it now. The token is a getter because the agent changes it
// while it runs: the server issues a new one at registration, and an agent the
// server forgot enrolls again.
func bindSlurmEnrollment(base SlurmEnrollment, server string, token func() string, stateDir string) *SlurmEnrollment {
	base.Server, base.Token, base.StateDir = server, token, stateDir
	return &base
}

// slurmNode is what turning a node into a guest of Slurm needs: the engine
// that borrows, the occupancy that reports other jobs, and what the server
// accepts (its rebinding of instances to the cards Slurm gave).
type slurmNode struct {
	log     *slog.Logger
	engine  *engine.Engine
	holders *atomic.Pointer[slurm.Leaser]
	accepts *atomic.Pointer[func(string) bool]
	prober  gpu.Prober
	getenv  func(string) string
	// enrolled is set for an agent a cluster entry installed; latest is the
	// entry's settings as the server last said them.
	enrolled *SlurmEnrollment
	latest   atomic.Pointer[slurm.Settings]
}

// startSlurm makes the node a guest of Slurm when Slurm allocates its GPUs.
// Only NVIDIA cards are matched to Slurm's device list so far; a Kubernetes
// node has its own allocator. accepts is set to the current connection's
// ServerAccepts as connections come and go.
func startSlurm(ctx context.Context, n *NodeRuntime, prober gpu.Prober, accepts *atomic.Pointer[func(string) bool], getenv func(string) string, enrolled *SlurmEnrollment) {
	mode := strings.ToLower(strings.TrimSpace(getenv("GPUWORKS_SLURM")))
	if mode == "off" || n.kubeNode || n.engine == nil || prober == nil {
		return
	}
	if prober.Vendor() != accelerator.VendorNVIDIA {
		if mode == "on" {
			n.log.Warn("Slurm card allocation is supported for NVIDIA cards only; the node's cards stay the platform's", "vendor", prober.Vendor())
		}
		return
	}
	s := &slurmNode{log: n.log, engine: n.engine, holders: &n.occupancy.slurm, accepts: accepts, prober: prober, getenv: getenv, enrolled: enrolled}
	node := strings.TrimSpace(getenv("GPUWORKS_SLURM_NODE"))
	if enrolled != nil {
		node = enrolled.Node
		settings := enrolled.Settings
		s.latest.Store(&settings)
		s.startReporting(ctx)
	}
	if node == "" {
		host, _ := os.Hostname()
		node, _, _ = strings.Cut(host, ".")
	}
	if mode != "on" {
		if _, err := exec.LookPath("scontrol"); err != nil {
			return // no Slurm on this host
		}
	}
	// The first answer is waited for, so instances the server replays at
	// connect already borrow their cards. A controller that cannot be
	// reached yet (the node booted before it) is asked again in the
	// background rather than read as "not a Slurm node".
	ok, err := s.detect(ctx, node)
	if err == nil {
		if ok {
			s.enable(ctx, node)
		} else if mode == "on" {
			s.log.Warn("Slurm knows this node but allocates no GPUs on it; the node's cards stay the platform's", "node", node)
		}
		return
	}
	s.log.Warn("Slurm is installed but could not be asked about this node; asking again", "node", node, "err", err)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			ok, err := s.detect(ctx, node)
			if err != nil {
				continue
			}
			if ok {
				s.enable(ctx, node)
			}
			return
		}
	}()
}

func (s *slurmNode) detect(ctx context.Context, node string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return slurm.Detect(ctx, slurm.ExecRunner, node)
}

// leaserConfig is how the node borrows cards: from the cluster entry's
// settings when an entry's health check installed this agent, from the
// environment otherwise.
func (s *slurmNode) leaserConfig(node string) slurm.Config {
	if s.enrolled != nil {
		// The entry decides, not the environment; the file the health check
		// leaves for an agent that cannot run slurmd itself is read first.
		cfg := slurm.Config{Node: node, OrderFile: filepath.Join(s.enrolled.StateDir, SlurmOrderFile),
			SubmittersFile: filepath.Join(s.enrolled.StateDir, SlurmSubmittersFile)}
		if latest := s.latest.Load(); latest != nil {
			cfg = slurm.ConfigWith(cfg, *latest)
		}
		return cfg
	}
	return slurm.Config{
		Node:           node,
		SbatchArgs:     strings.Fields(s.getenv("GPUWORKS_SLURM_SBATCH_ARGS")),
		PendingTimeout: envDuration(s.getenv, "GPUWORKS_SLURM_PENDING_TIMEOUT"),
		IdleReturn:     envDuration(s.getenv, "GPUWORKS_SLURM_IDLE_RETURN"),
	}
}

func (s *slurmNode) enable(ctx context.Context, node string) {
	cfg := s.leaserConfig(node)
	leaser := slurm.New(cfg, slurm.ExecRunner, s.prober.Probe, "/proc", s.log)
	s.holders.Store(leaser)
	s.engine.SetCardLeaser(leaser, func() bool {
		accepts := s.accepts.Load()
		return accepts != nil && (*accepts)(proto.CapInstanceCardRebind)
	})
	s.log.Info("Slurm allocates this node's cards; instances borrow them with placeholder jobs", "node", node, "sbatch_args", cfg.SbatchArgs, "account_user", cfg.AccountUser)
	go leaser.Run(ctx, slurmSweepInterval, s.engine.InstanceCards, func(instance, message string) {
		s.engine.LeaseLost(ctx, instance, message)
	})
}

func envDuration(getenv func(string) string, name string) time.Duration {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
}
