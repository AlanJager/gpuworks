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
