package slurm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A Slurm node's agent is installed by the cluster's health check with the
// cluster entry's enrollment token, not a node token: on every start it
// trades the token for this node's identity (POST /api/v1/slurm/enroll), the
// way a Kubernetes agent does. The server answers the same node every time,
// so asking on every start also picks up a changed entry setting, and a node
// the server cannot be asked about keeps running on the identity it saved.

// IdentityFile is where an enrolled agent keeps its node token, under the
// state directory.
const IdentityFile = "slurm-node-token.json"

// Settings are the entry's settings an agent applies to its placeholder jobs:
// the enrollment answer carries them and so does every report's.
type Settings = proto.SlurmAgentSettings

// Identity is what enrollment gives a Slurm node.
type Identity struct {
	NodeUUID  string   `json:"node_uuid"`
	NodeToken string   `json:"node_token"`
	Settings  Settings `json:"settings"`
	// Entry names the cluster entry that issued the identity, by its
	// enrollment token (EntryOf): the identity is only this agent's when the
	// entry it is configured with issued it.
	Entry string `json:"entry,omitempty"`
}

// EntryOf names an entry by its enrollment token without keeping the token.
func EntryOf(enrollToken string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(enrollToken)))
	return hex.EncodeToString(sum[:16])
}

// ReadIdentity is the identity this state directory saved when the entry with
// enrollToken issued it, else a zero Identity: another entry's identity is not
// this agent's to run on.
func ReadIdentity(stateDir, enrollToken string) Identity {
	id, err := readIdentity(filepath.Join(stateDir, IdentityFile))
	if err != nil || id.Entry != EntryOf(enrollToken) {
		return Identity{}
	}
	return id
}

// EnrollRefusal is why the server refused this node's enrollment, named by
// the product error's params.reason (proto.SlurmEnroll*). Only an unknown or
// revoked token still lets a node that enrolled earlier keep running on the
// identity it saved: the other two describe this node's own identity clashing
// with another cluster's or entry's, which an old token does not excuse away.
type EnrollRefusal struct {
	// Reason is the server's reason code, or "" when the 403 body carries none.
	Reason string
	// Message is the server's own English text, when it sent one.
	Message string
	// Entry names the entry holding the node, for proto.SlurmEnrollNodeTaken.
	Entry string
}

func (e *EnrollRefusal) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "the server refused this node's enrollment: " + e.Reason
}

func (e *EnrollRefusal) fallsBack() bool {
	return e.Reason == "" || e.Reason == proto.SlurmEnrollUnknownToken
}

// parseEnrollRefusal reads a 403 body for the server's reason. A body that is
// not the server's own refusal (a proxy's 403) is a plain error, treated like
// a network failure: it says nothing about this node's identity.
func parseEnrollRefusal(raw []byte) error {
	var body struct {
		Error  string `json:"error"`
		Code   string `json:"code"`
		Params struct {
			Reason string `json:"reason"`
			Entry  string `json:"entry"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Code != proto.ErrorSlurmEnrollRefused {
		return errors.New("the server refused this cluster's enrollment token: " + strings.TrimSpace(string(raw)))
	}
	return &EnrollRefusal{Reason: body.Params.Reason, Message: body.Error, Entry: body.Params.Entry}
}

// Enroll trades the entry's enrollment token for this node's identity and
// saves it. When the server gives no answer, or a refusal that is not about
// this node's own identity (EnrollRefusal.fallsBack), it falls back to the
// identity this entry saved earlier, unless fallback is false: the caller
// passes false once the server has refused this node's identity, so a later
// outage does not hide the refusal behind the saved identity.
func Enroll(ctx context.Context, httpc *http.Client, serverURL, enrollToken, slurmCluster, nodeName, stateDir string, fallback bool) (Identity, error) {
	path := filepath.Join(stateDir, IdentityFile)
	saved, savedErr := readIdentity(path)
	entry := EntryOf(enrollToken)
	id, err := enroll(ctx, httpc, serverURL, enrollToken, slurmCluster, nodeName)
	if err != nil {
		var refusal *EnrollRefusal
		if errors.As(err, &refusal) && !refusal.fallsBack() {
			return Identity{}, err
		}
		if fallback && savedErr == nil && saved.Entry == entry && saved.NodeToken != "" {
			return saved, nil
		}
		return Identity{}, err
	}
	id.Entry = entry
	if id != saved {
		if err := writeIdentity(path, id); err != nil {
			return Identity{}, err
		}
	}
	return id, nil
}

func enroll(ctx context.Context, httpc *http.Client, serverURL, enrollToken, slurmCluster, nodeName string) (Identity, error) {
	body, _ := json.Marshal(proto.SlurmEnrollRequest{Token: enrollToken, SlurmClusterName: slurmCluster, NodeName: nodeName})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/api/v1/slurm/enroll", bytes.NewReader(body))
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
		return Identity{}, parseEnrollRefusal(raw)
	}
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("enroll with %s: HTTP %d: %s", serverURL, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var answer proto.SlurmEnrollResponse
	if err := json.Unmarshal(raw, &answer); err != nil || answer.NodeToken == "" {
		return Identity{}, fmt.Errorf("enroll with %s: unexpected answer %q", serverURL, raw)
	}
	return Identity{NodeUUID: answer.NodeUUID, NodeToken: answer.NodeToken, Settings: answer.Settings}, nil
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

// ClusterName is Slurm's own ClusterName, from `scontrol show config`: what
// the enrollment binds an entry's token to.
func ClusterName(ctx context.Context, run Runner) (string, error) {
	out, err := run(ctx, "scontrol", "show", "config")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "ClusterName" {
			if name := strings.TrimSpace(value); name != "" && name != "(null)" {
				return name, nil
			}
		}
	}
	return "", errors.New("scontrol show config names no ClusterName")
}
