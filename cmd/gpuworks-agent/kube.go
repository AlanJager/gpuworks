package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/kubeboot"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// enrollKubeNode makes an agent of a cluster's DaemonSet a node: it trades
// the cluster's enrollment token for this node's token, retrying until the
// server answers, since the DaemonSet may be applied before the server can
// be reached.
func enrollKubeNode(ctx context.Context, log *slog.Logger, cfg agentapp.Config) (kube.Identity, error) {
	enrollToken := strings.TrimSpace(os.Getenv("GPUWORKS_KUBE_ENROLL_TOKEN"))
	nodeName := strings.TrimSpace(os.Getenv("GPUWORKS_KUBE_NODE_NAME"))
	if enrollToken == "" || nodeName == "" {
		return kube.Identity{}, fmt.Errorf("--driver=kube without --token needs GPUWORKS_KUBE_ENROLL_TOKEN and GPUWORKS_KUBE_NODE_NAME (the DaemonSet manifest sets them)")
	}
	api, err := kubeapi.InCluster()
	if err != nil {
		return kube.Identity{}, err
	}
	httpc := &http.Client{Timeout: 30 * time.Second}
	delay := 2 * time.Second
	for {
		id, err := kube.Enroll(ctx, api, httpc, cfg.ServerURL, enrollToken, nodeName, cfg.StateDir)
		if err == nil {
			log.Info("enrolled as a Kubernetes node", "node", nodeName, "uuid", id.NodeUUID)
			return id, nil
		}
		log.Warn("enrollment failed; retrying", "err", err, "in", delay.String())
		select {
		case <-ctx.Done():
			return kube.Identity{}, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, time.Minute)
	}
}

// alignKubeAgentTimeout bounds taking the server's binary on a first start:
// the agent registers with the binary it has rather than wait longer.
const alignKubeAgentTimeout = 3 * time.Minute

// alignKubeAgent runs before a Kubernetes agent registers. An agent the
// image's binary handed over to (kubeboot) takes the server's own binary
// once after the state directory was seeded, so a new node runs the
// server's version as an installed agent does. One that kept running the
// image's binary says why: its upgrades then change the DaemonSet's image.
func alignKubeAgent(log *slog.Logger, serverURL, token string) {
	if reason := os.Getenv(kubeboot.UnavailableEnv); reason != "" {
		log.Warn("running the agent image's own binary; upgrading this node changes the DaemonSet image", "reason", reason)
		return
	}
	if !kubeboot.TakeAlign() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), alignKubeAgentTimeout)
	defer cancel()
	// Returns only when the binaries are the same or the replacement failed.
	if err := agentconn.AlignWithServer(ctx, serverURL, token); err != nil {
		log.Warn("could not take the server's agent binary; running the image's", "err", err)
	}
}
