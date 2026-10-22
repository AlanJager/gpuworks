// Package fleet is the control-plane load driver: many agents against one
// server, measuring what the fleet costs the control plane rather than what any
// single deployment does.
//
// The driver is shared by both shapes of the case. One runs the server and the
// whole fleet inside the test process; the other runs the real server binary as
// its own process and the fleet as a second one. Everything between the moment
// the nodes are online and the moment the churn ends is the same code, so the
// two shapes differ only in what they are measuring and not in what they do.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is the operator's view of the control plane: an HTTP client holding a
// login token. Both shapes talk to a real HTTP server, so neither needs
// privileged access to the database to drive or observe the workload.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// Login exchanges an administrator password for a bearer token. The timeout is
// generous because a saturated control plane is exactly what is being measured:
// a read that takes half a minute is a result, not a transport failure.
func Login(base, user, password string) (*Client, error) {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	client := &Client{base: base, http: &http.Client{Timeout: 5 * time.Minute}}
	body, err := json.Marshal(map[string]string{"username": user, "password": password})
	if err != nil {
		return nil, err
	}
	resp, err := client.http.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("login: status %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Token == "" {
		return nil, fmt.Errorf("login returned no token")
	}
	client.token = out.Token
	return client, nil
}

// BaseURL is the control plane this client talks to.
func (c *Client) BaseURL() string { return c.base }

func (c *Client) do(method, path string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// Get issues a read and hands the caller the open response; the caller closes it.
func (c *Client) Get(path string) (*http.Response, error) { return c.do(http.MethodGet, path, nil) }

// Discard issues a read and drains it, which is what a browser poll costs the
// server. It returns how long the whole exchange took.
func (c *Client) Discard(path string) (time.Duration, error) {
	return c.discard(path, nil)
}

func (c *Client) discard(path string, started func()) (time.Duration, error) {
	start := time.Now()
	if started != nil {
		started()
	}
	ctx, cancel := context.WithTimeout(context.Background(), ListReadBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return time.Since(start), err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return time.Since(start), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return time.Since(start), fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return time.Since(start), err
}

// PostJSON posts a body and decodes the response into out when out is non-nil.
func (c *Client) PostJSON(path string, body []byte, out any) error {
	resp, err := c.do(http.MethodPost, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// GetJSON reads and decodes a response body.
func (c *Client) GetJSON(path string, out any) error {
	resp, err := c.Get(path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Delete removes a resource and drains the reply.
func (c *Client) Delete(path string) error {
	resp, err := c.do(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// NodeIdentity is what the control plane hands back when a node is enrolled:
// the uuid it is addressed by and the token its agent authenticates with.
type NodeIdentity struct {
	Name  string `json:"name"`
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

// CreateNode enrols one node. A separate fleet process has no other way in:
// the token is issued here and never exists in the database in plaintext form
// the test could read back.
func (c *Client) CreateNode(name string) (NodeIdentity, error) {
	body, err := json.Marshal(map[string]string{"name": name, "server_url": c.base})
	if err != nil {
		return NodeIdentity{}, err
	}
	var out struct {
		Node  NodeIdentity `json:"node"`
		Token string       `json:"token"`
	}
	if err := c.PostJSON("/api/v1/nodes", body, &out); err != nil {
		return NodeIdentity{}, err
	}
	if out.Node.UUID == "" || out.Token == "" {
		return NodeIdentity{}, fmt.Errorf("create node %q returned no identity", name)
	}
	return NodeIdentity{Name: name, UUID: out.Node.UUID, Token: out.Token}, nil
}

// RegisterLocalModel records weights that are already on a node as a model the
// platform can deploy. The control plane reads the directory over node RPC, so
// this exercises the product's own path rather than seeding the database.
func (c *Client) RegisterLocalModel(nodeUUID, path, name string) (uint, error) {
	body, err := json.Marshal(map[string]any{
		"node_uuid": nodeUUID, "path": path, "source_id": name, "name": name,
	})
	if err != nil {
		return 0, err
	}
	var out struct {
		ID uint `json:"id"`
	}
	if err := c.PostJSON("/api/v1/models/local", body, &out); err != nil {
		return 0, err
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("register local model on %s returned no id", nodeUUID)
	}
	return out.ID, nil
}

// RegisterModelPath records the same already-present weights on a further node.
func (c *Client) RegisterModelPath(modelID uint, nodeUUID, path string) error {
	body, err := json.Marshal(map[string]any{"node_uuid": nodeUUID, "path": path})
	if err != nil {
		return err
	}
	return c.PostJSON(fmt.Sprintf("/api/v1/models/%d/register-path", modelID), body, nil)
}

// CloseIdleConnections drops the keep-alive connections the churn left behind.
// A load case opens hundreds of them, and an HTTP server cannot finish shutting
// down while its client still holds them open.
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }
