package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/slurm"
	"gpuworks.ai/gpuworks/internal/agentconf"
)

// slurmRunner runs the Slurm CLI; a test substitutes its own.
var slurmRunner slurm.Runner = slurm.ExecRunner

// slurmEnrollRetryDelay is the first wait between enrollment attempts (it
// doubles up to a minute); a test shortens it.
var slurmEnrollRetryDelay = 2 * time.Second

// slurmAcquireToken makes an agent a health check installed a node: with no
// node token in conf, it trades the cluster entry's enrollment token for one,
// storing it onto conf and the entry's settings onto cfg. An agent that has a
// token (installed by hand, or by an installer of any other kind) is left as
// it is.
func slurmAcquireToken(log *slog.Logger, conf *agentconf.Config, cfg *agentapp.Config, stateDir string) error {
	if strings.TrimSpace(conf.Token) != "" || conf.SlurmEnrollToken == "" {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	id, err := enrollSlurmNode(ctx, log, slurmRunner, conf.Server, conf.SlurmEnrollToken, conf.SlurmNode, stateDir)
	if err != nil {
		return err
	}
	conf.Token = id.NodeToken
	cfg.Slurm = &agentapp.SlurmEnrollment{Node: conf.SlurmNode, Settings: id.Settings}
	// A node the server forgot (deleted, or its token regenerated) enrolls
	// again by itself: the saved identity is no excuse once the server has
	// said it does not know the token.
	httpc := &http.Client{Timeout: 30 * time.Second}
	server, enrollToken, node := conf.Server, conf.SlurmEnrollToken, conf.SlurmNode
	cfg.Reenroll = func(ctx context.Context) (string, error) {
		id, err := enrollSlurmOnce(ctx, httpc, slurmRunner, server, enrollToken, node, stateDir, false)
		return id.NodeToken, err
	}
	return nil
}

// useSavedSlurmIdentity gives a command that only talks to the server
// (upgrade, the connectivity doctor) the node token an enrolled agent saved,
// without enrolling.
func useSavedSlurmIdentity(conf *agentconf.Config, stateDir string) {
	if strings.TrimSpace(conf.Token) == "" && conf.SlurmEnrollToken != "" {
		conf.Token = slurm.ReadIdentity(stateDir, conf.SlurmEnrollToken).NodeToken
	}
}

// enrollSlurmNode trades the entry's enrollment token for this node's token,
// retrying until the server answers: the health check installs the agent
// before the server is necessarily reachable from the node. Slurm's own
// ClusterName, which the entry binds to on the first enrollment, is asked of
// the node's slurmctld each attempt (it may not answer yet either).
func enrollSlurmNode(ctx context.Context, log *slog.Logger, run slurm.Runner, server, enrollToken, node, stateDir string) (slurm.Identity, error) {
	httpc := &http.Client{Timeout: 30 * time.Second}
	delay := slurmEnrollRetryDelay
	fallback := true
	for {
		id, err := enrollSlurmOnce(ctx, httpc, run, server, enrollToken, node, stateDir, fallback)
		var refusal *slurm.EnrollRefusal
		if errors.As(err, &refusal) {
			fallback = false // a refusal of this node's identity is not undone by a later outage
		}
		if err == nil {
			log.Info("enrolled as a Slurm node", "node", node, "uuid", id.NodeUUID)
			return id, nil
		}
		log.Warn("enrollment failed; retrying", "err", err, "in", delay.String())
		select {
		case <-ctx.Done():
			return slurm.Identity{}, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, time.Minute)
	}
}

func enrollSlurmOnce(ctx context.Context, httpc *http.Client, run slurm.Runner, server, enrollToken, node, stateDir string, fallback bool) (slurm.Identity, error) {
	askCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cluster, err := slurm.ClusterName(askCtx, run)
	if err != nil {
		// Slurm cannot be asked (the controller is not up yet): an agent that
		// enrolled before keeps its identity, one that never did waits.
		if saved := slurm.ReadIdentity(stateDir, enrollToken); fallback && saved.NodeToken != "" {
			return saved, nil
		}
		return slurm.Identity{}, err
	}
	return slurm.Enroll(ctx, httpc, server, enrollToken, cluster, node, stateDir, fallback)
}
