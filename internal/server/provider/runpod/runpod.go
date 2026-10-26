// Package runpod implements the RunPod cloud-provider driver.
//
// RunPod exposes REST API v2 (base https://api.runpod.io/v2) authenticated
// with a bearer API key. RunPod "pods" are containers, so agent installation
// uses the udocker path (Kind="container").
package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

const (
	defaultBaseURL = provider.RunPodEndpoint
)

func init() {
	provider.Register(New())
}

// Driver is the RunPod provider driver.
type Driver struct {
	baseURL string
	http    *http.Client
	// repeatAfter replaces the one-second pause before a repeated request.
	repeatAfter time.Duration
	// portWaitTimeout and portPollInterval bound the wait for a pod restarted
	// to expose a port (tests shorten them); listening replaces the SSH read
	// of a pod's listening ports in tests.
	portWaitTimeout  time.Duration
	portPollInterval time.Duration
	listening        func(ctx context.Context, inst provider.Instance, auth provision.SSHAuth) (map[int]bool, error)
	// loginHook replaces the SSH login check in tests.
	loginHook func(ctx context.Context, inst provider.Instance, auth provision.SSHAuth) error
}

// GetInstance refreshes one Pod through the detail endpoint. RunPod's list
// response can lag behind the Pod runtime and omit direct SSH mappings.
func (d *Driver) GetInstance(ctx context.Context, creds provider.Credentials, externalID string) (provider.Instance, error) {
	p, err := d.getPod(ctx, creds, externalID)
	if err != nil {
		return provider.Instance{}, err
	}
	return p.toInstance(), nil
}

// New returns a RunPod driver pointed at the real API.
func New() *Driver {
	return &Driver{
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Name implements provider.Driver.
func (d *Driver) Name() string { return "runpod" }

// Capabilities implements provider.Driver.
func (d *Driver) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		CanListInstances: true,
		CanPowerControl:  false,
		CanInstallAgent:  true,
		CanCreate:        true,
		CanDelete:        true,
		HasPricing:       true,
	}
}

// pod is the tolerant on-the-wire shape of a RunPod pod. All fields are
// optional; missing fields decode to their zero values.
type pod struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Status        string          `json:"status"`
	DesiredStatus string          `json:"desiredStatus"`
	Cost          float64         `json:"cost"`
	CostPerHr     float64         `json:"costPerHr"`
	DataCenterID  string          `json:"dataCenterId"`
	GPUCount      int             `json:"gpuCount"`
	GPU           json.RawMessage `json:"gpu"`
	GPUTypeID     string          `json:"gpuTypeId"`
	GPUTypeIDs    []string        `json:"gpuTypeIds"`
	GPUType       json.RawMessage `json:"gpuType"`
	PublicIP      string          `json:"publicIp"`
	MachineID     string          `json:"machineId"`
	Machine       json.RawMessage `json:"machine"`
	PortMappings  json.RawMessage `json:"portMappings"`
	Ports         json.RawMessage `json:"ports"`
	Runtime       json.RawMessage `json:"runtime"`
	SSH           struct {
		Direct podSSHConnection `json:"direct"`
		Proxy  podSSHConnection `json:"proxy"`
	} `json:"ssh"`
	Env json.RawMessage `json:"env"`
}

type podSSHConnection struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
}

// gpuModel resolves the GPU model from the top-level field or the nested
// machine object.
func (p *pod) gpuModel() string {
	if len(p.GPU) > 0 {
		var gpu struct {
			ID      string `json:"id"`
			Display string `json:"displayName"`
		}
		if err := json.Unmarshal(p.GPU, &gpu); err == nil {
			if gpu.Display != "" {
				return gpu.Display
			}
			if gpu.ID != "" {
				return gpu.ID
			}
		}
	}
	if p.GPUTypeID != "" {
		return p.GPUTypeID
	}
	if len(p.GPUTypeIDs) > 0 && p.GPUTypeIDs[0] != "" {
		return p.GPUTypeIDs[0]
	}
	for _, raw := range []json.RawMessage{p.GPUType, p.Machine} {
		if len(raw) == 0 {
			continue
		}
		var scalar string
		if err := json.Unmarshal(raw, &scalar); err == nil && scalar != "" {
			return scalar
		}
		var m struct {
			GPUTypeID  string `json:"gpuTypeId"`
			Display    string `json:"displayName"`
			Name       string `json:"name"`
			GPUDisplay string `json:"gpuDisplayName"`
			GPUType    struct {
				Display string `json:"displayName"`
				Name    string `json:"name"`
			} `json:"gpuType"`
		}
		if err := json.Unmarshal(raw, &m); err == nil {
			for _, value := range []string{m.GPUTypeID, m.GPUDisplay, m.Display, m.Name, m.GPUType.Display, m.GPUType.Name} {
				if value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func (p *pod) gpuCount() int {
	if len(p.GPU) > 0 {
		var gpu struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(p.GPU, &gpu); err == nil && gpu.Count > 0 {
			return gpu.Count
		}
	}
	if p.GPUCount > 0 {
		return p.GPUCount
	}
	return len(p.GPUTypeIDs)
}

func (p *pod) power() provider.PowerState {
	status := firstNonEmpty(p.Status, p.DesiredStatus)
	if strings.EqualFold(status, "RUNNING") {
		return provider.PowerRunning
	}
	switch strings.ToUpper(status) {
	case "EXITED", "TERMINATED", "STOPPED":
		return provider.PowerStopped
	}
	return provider.PowerUnknown // CREATED, RESTARTING, ... or none
}

func (p *pod) toInstance() provider.Instance {
	name := p.Name
	if name == "" {
		name = p.ID
	}
	price := p.Cost
	if price == 0 {
		price = p.CostPerHr
	}
	inst := provider.Instance{
		ExternalID: p.ID,
		Name:       name,
		GPUModel:   p.gpuModel(),
		GPUCount:   p.gpuCount(),
		Power:      p.power(),
		PricePerHr: price,
		Region:     p.DataCenterID,
		Kind:       "container",
	}
	if host, port, user := p.sshEndpoint(); host != "" && port != 0 {
		inst.SSHConnections = append(inst.SSHConnections, provider.SSHConnection{
			ID: "direct", Type: "direct", Label: provider.SSHConnectionLabel("direct"), Host: host, Port: port, User: user, InstallSupported: true,
		})
	}
	if proxy := p.SSH.Proxy; proxy.Host != "" && proxy.Port > 0 {
		inst.SSHConnections = append(inst.SSHConnections, provider.SSHConnection{
			ID: "proxy", Type: "proxy", Label: provider.SSHConnectionLabel("proxy"), Host: proxy.Host, Port: proxy.Port,
			User: firstNonEmpty(proxy.Username, "root"), InstallSupported: false,
		})
	}
	provider.SetPreferredSSHConnection(&inst)
	return inst
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// do performs an authenticated request against the RunPod API.
func (d *Driver) apiBaseURL(creds provider.Credentials) string {
	baseURL := strings.TrimSpace(creds.Endpoint)
	if strings.Trim(baseURL, "/") == "" {
		baseURL = d.baseURL
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil {
		return baseURL // The request builder reports the malformed endpoint.
	}
	// Only literal path separators are trimmed: a trailing slash in a query
	// value or an escaped slash inside a proxy path segment is data.
	escapedPath := strings.TrimRight(endpoint.EscapedPath(), "/")
	endpoint.Path, err = url.PathUnescape(escapedPath)
	if err != nil {
		return baseURL
	}
	endpoint.RawPath = escapedPath
	return endpoint.String()
}

func (d *Driver) do(ctx context.Context, creds provider.Credentials, method, path string) (*http.Response, error) {
	return d.doJSON(ctx, creds, method, path, nil)
}

// requestAttempts is how many times a request that can safely be repeated is
// sent when the connection itself fails. RunPod is usually reached through an
// egress proxy that now and then drops a connection (an EOF); a pod listing
// or a delete that is repeated does no harm, a create that is repeated might
// start a second billed pod, so only GET and DELETE are.
const requestAttempts = 3

func (d *Driver) doJSON(ctx context.Context, creds provider.Credentials, method, path string, body any) (*http.Response, error) {
	endpoint, err := url.Parse(d.apiBaseURL(creds))
	if err != nil {
		return nil, err
	}
	// Keep the endpoint's query attached to the resulting operation URL. Both
	// the proxy prefix and escaped instance IDs retain their path encoding.
	escapedPath := endpoint.EscapedPath() + path
	endpoint.Path, err = url.PathUnescape(escapedPath)
	if err != nil {
		return nil, err
	}
	endpoint.RawPath = escapedPath
	var payload []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("runpod: encode request: %w", err)
		}
		payload = data
	}
	attempts := 1
	if method == http.MethodGet || method == http.MethodDelete {
		attempts = requestAttempts
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+creds.APIKey)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := d.http.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt == attempts || ctx.Err() != nil {
			break
		}
		select {
		case <-time.After(time.Duration(attempt) * d.retryDelay()):
		case <-ctx.Done():
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// retryDelay is the pause before the first repeat; tests shorten it.
func (d *Driver) retryDelay() time.Duration {
	if d.repeatAfter > 0 {
		return d.repeatAfter
	}
	return time.Second
}

// ValidateCredentials implements provider.Driver.
func (d *Driver) ValidateCredentials(ctx context.Context, creds provider.Credentials) error {
	resp, err := d.do(ctx, creds, http.MethodGet, "/pods")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("runpod: invalid credentials (status %d)", resp.StatusCode)
	default:
		return fmt.Errorf("runpod: validate credentials: unexpected status %d", resp.StatusCode)
	}
}

// ListInstances implements provider.Driver. Error bodies are untrusted and may
// echo credentials; return operation/status only, as for other failed requests.
func (d *Driver) ListInstances(ctx context.Context, creds provider.Credentials) ([]provider.Instance, error) {
	resp, err := d.do(ctx, creds, http.MethodGet, "/pods")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("runpod: list pods: status %d", resp.StatusCode)
	}
	pods, err := decodePods(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("runpod: decode pods: %w", err)
	}
	out := make([]provider.Instance, 0, len(pods))
	for i := range pods {
		out = append(out, pods[i].toInstance())
	}
	return out, nil
}

func (d *Driver) getPod(ctx context.Context, creds provider.Credentials, id string) (pod, error) {
	resp, err := d.do(ctx, creds, http.MethodGet, "/pods/"+url.PathEscape(id))
	if err != nil {
		return pod{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return pod{}, fmt.Errorf("runpod: get pod %q: status %d", id, resp.StatusCode)
	}
	var out pod
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return pod{}, fmt.Errorf("runpod: decode pod %q: %w", id, err)
	}
	return out, nil
}

// decodePods tolerates either a bare array or a {"pods":[...]}/{"data":[...]}
// envelope. Any other shape is an error rather than an empty list: sync
// removes rows for pods it does not list.
func decodePods(r io.Reader) ([]pod, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) > 0 && data[0] == '[' {
		var pods []pod
		if err := json.Unmarshal(data, &pods); err != nil {
			return nil, err
		}
		return pods, nil
	}
	var env struct {
		Pods *[]pod `json:"pods"`
		Data *[]pod `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	switch {
	case env.Pods != nil && len(*env.Pods) > 0:
		return *env.Pods, nil
	case env.Data != nil:
		return *env.Data, nil
	case env.Pods != nil:
		return *env.Pods, nil
	}
	return nil, fmt.Errorf("response has neither a pod array nor a pods/data envelope")
}

// PowerState implements provider.Driver.
func (d *Driver) PowerState(ctx context.Context, creds provider.Credentials, externalID string) (provider.PowerState, error) {
	p, err := d.getPod(ctx, creds, externalID)
	if err != nil {
		return provider.PowerUnknown, err
	}
	return p.power(), nil
}

func (d *Driver) PowerOn(ctx context.Context, creds provider.Credentials, externalID string) error {
	return provider.ErrUnsupported
}

func (d *Driver) PowerOff(ctx context.Context, creds provider.Credentials, externalID string) error {
	return provider.ErrUnsupported
}

func (d *Driver) CreateInstance(ctx context.Context, creds provider.Credentials, req provider.CreateInstanceRequest) (provider.Instance, error) {
	if strings.TrimSpace(req.GPUModel) == "" {
		return provider.Instance{}, errors.New("runpod: GPU model is required")
	}
	gpuCount := req.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "gpuworks"
	}
	image := strings.TrimSpace(req.Image)
	if image == "" {
		image = "runpod/pytorch:1.0.2-cu1281-torch280-ubuntu2404"
	}
	diskGB := positiveInt(req.Extra["disk_gb"], 20)
	// Expose the agent's default port with SSH: exposing it later edits the
	// pod, which restarts it (server_dial.go).
	ports := []string{"22/tcp", strconv.Itoa(defaultAgentPort) + "/tcp"}
	if raw := strings.TrimSpace(req.Extra["ports"]); raw != "" {
		ports = strings.Split(raw, ",")
	}
	env := map[string]string{}
	for key, value := range req.Extra {
		if strings.HasPrefix(key, "env.") {
			env[strings.TrimPrefix(key, "env.")] = value
		}
	}
	body := map[string]any{
		"name": name, "image": image, "gpu": map[string]any{"id": req.GPUModel, "count": gpuCount},
		"disk": diskGB, "ports": ports, "env": env, "startSsh": true,
	}
	if volumeGB := positiveInt(req.Extra["volume_gb"], 0); volumeGB > 0 {
		body["mounts"] = map[string]any{"persistent": map[string]any{"size": volumeGB, "path": firstNonEmpty(req.Extra["volume_mount_path"], "/workspace")}}
	}
	resp, err := d.doJSON(ctx, creds, http.MethodPost, "/pods", body)
	if err != nil {
		return provider.Instance{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return provider.Instance{}, fmt.Errorf("runpod: create pod: status %d", resp.StatusCode)
	}
	p, err := decodePod(resp.Body)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("runpod: decode created pod: %w", err)
	}
	if p.ID == "" {
		return provider.Instance{}, errors.New("runpod: create pod returned an empty id")
	}
	return p.toInstance(), nil
}

func (d *Driver) DeleteInstance(ctx context.Context, creds provider.Credentials, externalID string) error {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return errors.New("runpod: pod id is required")
	}
	resp, err := d.do(ctx, creds, http.MethodDelete, "/pods/"+url.PathEscape(externalID))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("runpod: delete pod %q: status %d", externalID, resp.StatusCode)
}

func decodePod(r io.Reader) (pod, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return pod{}, err
	}
	var direct pod
	if err := json.Unmarshal(data, &direct); err != nil {
		return pod{}, err
	}
	if direct.ID != "" {
		return direct, nil
	}
	var env struct {
		Pod  pod `json:"pod"`
		Data pod `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return pod{}, err
	}
	if env.Pod.ID != "" {
		return env.Pod, nil
	}
	return env.Data, nil
}

func positiveInt(raw string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func (d *Driver) ResizeInstance(ctx context.Context, creds provider.Credentials, externalID string, req provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}
