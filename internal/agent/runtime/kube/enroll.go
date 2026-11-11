package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// IdentityFile is where an enrolled agent keeps its node token, under the
// state directory (a hostPath, so it outlives the agent pod).
const IdentityFile = "kube-node-token.json"

// Identity is what enrollment gives a Kubernetes node.
type Identity struct {
	NodeUUID          string `json:"node_uuid"`
	NodeToken         string `json:"node_token"`
	IdleReturnSeconds int    `json:"idle_return_seconds"`
}

// IdleReturn is the cluster's grace period for borrowed cards; negative
// keeps them.
func (id Identity) IdleReturn() time.Duration {
	if id.IdleReturnSeconds < 0 {
		return -1
	}
	return time.Duration(id.IdleReturnSeconds) * time.Second
}

// Enroll trades the cluster's enrollment token for this node's identity.
// The server answers the same node every time, so it asks on every start
// (picking up a changed cluster setting) and falls back to the identity it
// saved when it gets no answer — or a refusal: a revoked enrollment token
// stops new nodes joining, not the ones that already did, whose own node
// token is what the server checks. Only a node that never enrolled needs
// the server to say yes.
func Enroll(ctx context.Context, api *kubeapi.Client, httpc *http.Client, serverURL, enrollToken, nodeName, stateDir string) (Identity, error) {
	path := filepath.Join(stateDir, IdentityFile)
	saved, savedErr := readIdentity(path)
	id, err := enroll(ctx, api, httpc, serverURL, enrollToken, nodeName)
	if err != nil {
		if savedErr == nil {
			return saved, nil
		}
		return Identity{}, err
	}
	if id != saved {
		if err := writeIdentity(path, id); err != nil {
			return Identity{}, err
		}
	}
	return id, nil
}

func enroll(ctx context.Context, api *kubeapi.Client, httpc *http.Client, serverURL, enrollToken, nodeName string) (Identity, error) {
	var ns kubeapi.Namespace
	if err := api.Get(ctx, kubeapi.NamespacePath("kube-system"), &ns); err != nil {
		return Identity{}, fmt.Errorf("read the cluster's identity (kube-system namespace): %w", err)
	}
	body, _ := json.Marshal(map[string]string{"token": enrollToken, "cluster_uid": ns.Metadata.UID, "node_name": nodeName})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/api/v1/kube/enroll", bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("enroll with %s: %w", serverURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusForbidden {
		return Identity{}, errors.New("the server refused this cluster's enrollment token: " + strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("enroll with %s: HTTP %d: %s", serverURL, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil || id.NodeToken == "" {
		return Identity{}, fmt.Errorf("enroll with %s: unexpected answer %q", serverURL, raw)
	}
	return id, nil
}

func readIdentity(path string) (Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil || id.NodeToken == "" {
		return Identity{}, fmt.Errorf("%s is not a node identity", path)
	}
	return id, nil
}

func writeIdentity(path string, id Identity) error {
	raw, _ := json.Marshal(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
