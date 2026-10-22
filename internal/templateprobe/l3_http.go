package templateprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxManagementResponseBytes = int64(4 << 20)

type HTTPDeploymentClient struct {
	BaseURL         string
	ManagementToken string
	Client          *http.Client
	PollInterval    time.Duration
}

type apiDeployment struct {
	ID            uint           `json:"id"`
	ServedName    string         `json:"served_name"`
	RuntimeState  string         `json:"runtime_state"`
	State         string         `json:"state"`
	ModelProgress []apiModelFile `json:"model_progress"`
	Instances     []apiInstance  `json:"instances"`
}

type apiModelFile struct {
	State string `json:"state"`
}

type apiInstance struct {
	State         string            `json:"state"`
	ContainerID   string            `json:"container_id"`
	ImageDigestID string            `json:"image_digest_id"`
	Message       string            `json:"message"`
	Phase         string            `json:"phase"`
	NodeRank      int               `json:"node_rank"`
	Headless      bool              `json:"headless"`
	Entrypoint    []string          `json:"entrypoint"`
	Argv          []string          `json:"argv"`
	Env           map[string]string `json:"env"`
	ModelProgress *apiModelFile     `json:"model_progress"`
}

func NewHTTPDeploymentClient(baseURL, managementToken string, client *http.Client) (*HTTPDeploymentClient, error) {
	normalized, err := normalizeHTTPBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPDeploymentClient{
		BaseURL: normalized, ManagementToken: strings.TrimSpace(managementToken),
		Client: client, PollInterval: 2 * time.Second,
	}, nil
}

func (c *HTTPDeploymentClient) Create(ctx context.Context, deployment json.RawMessage) (DeploymentHandle, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/deployments", bytes.NewReader(deployment))
	if err != nil {
		return DeploymentHandle{}, HarnessStageError{Stage: "deployment_create", Err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	status, body, err := c.do(request)
	if err != nil {
		return DeploymentHandle{}, HarnessStageError{Stage: "deployment_create", Err: err}
	}
	if status < 200 || status >= 300 {
		return DeploymentHandle{}, HarnessStageError{Stage: "deployment_create", Err: managementStatusError(status, body)}
	}
	var deploymentResponse apiDeployment
	if err := json.Unmarshal(body, &deploymentResponse); err != nil {
		return DeploymentHandle{}, HarnessStageError{Stage: "deployment_create", Err: fmt.Errorf("decode response: %w", err)}
	}
	if deploymentResponse.ID == 0 {
		return DeploymentHandle{}, HarnessStageError{Stage: "deployment_create", Err: errors.New("response has no deployment id")}
	}
	return DeploymentHandle{ID: deploymentResponse.ID}, nil
}

func (c *HTTPDeploymentClient) WaitReady(ctx context.Context, handle DeploymentHandle) (DeploymentObservation, error) {
	if handle.ID == 0 {
		return DeploymentObservation{}, HarnessStageError{Stage: "deployment_poll", Err: errors.New("deployment id is zero")}
	}
	interval := c.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	lastPhase := "deployment_ready"
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return DeploymentObservation{}, HarnessStageError{Stage: "canceled", Err: ctx.Err()}
			}
			return DeploymentObservation{}, PlatformStageError{Stage: lastPhase, Err: ctx.Err()}
		case <-timer.C:
		}
		deployment, status, err := c.get(ctx, handle)
		if err != nil {
			return DeploymentObservation{}, HarnessStageError{Stage: "deployment_poll", Err: err}
		}
		if status == http.StatusNotFound {
			return DeploymentObservation{}, HarnessStageError{Stage: "deployment_poll", Err: errors.New("deployment disappeared")}
		}
		if status < 200 || status >= 300 {
			return DeploymentObservation{}, HarnessStageError{Stage: "deployment_poll", Err: fmt.Errorf("HTTP %d", status)}
		}
		if stage, message, failed := failedDeployment(deployment); failed {
			return DeploymentObservation{}, PlatformStageError{Stage: stage, Err: errors.New(message)}
		}
		if phase := deploymentPhase(deployment); phase != "" {
			lastPhase = phase
		}
		if observation, ready := c.readyObservation(deployment); ready {
			return observation, nil
		}
		timer.Reset(interval)
	}
}

func (c *HTTPDeploymentClient) Cleanup(ctx context.Context, handle DeploymentHandle) error {
	endpoint := c.BaseURL + "/api/v1/deployments/" + strconv.FormatUint(uint64(handle.ID), 10)
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	status, body, err := c.do(request)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status < 200 || status >= 300 {
		return managementStatusError(status, body)
	}
	interval := c.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, status, err := c.get(ctx, handle)
			if err != nil {
				return err
			}
			if status == http.StatusNotFound {
				return nil
			}
			if status < 200 || status >= 300 {
				return fmt.Errorf("cleanup poll returned HTTP %d", status)
			}
		}
	}
}

func (c *HTTPDeploymentClient) get(ctx context.Context, handle DeploymentHandle) (apiDeployment, int, error) {
	endpoint := c.BaseURL + "/api/v1/deployments/" + strconv.FormatUint(uint64(handle.ID), 10)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return apiDeployment{}, 0, err
	}
	status, body, err := c.do(request)
	if err != nil || status == http.StatusNotFound {
		return apiDeployment{}, status, err
	}
	if status < 200 || status >= 300 {
		return apiDeployment{}, status, nil
	}
	var deployment apiDeployment
	if err := json.Unmarshal(body, &deployment); err != nil {
		return apiDeployment{}, status, fmt.Errorf("decode deployment: %w", err)
	}
	return deployment, status, nil
}

func (c *HTTPDeploymentClient) do(request *http.Request) (int, []byte, error) {
	if c.ManagementToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.ManagementToken)
	}
	response, err := c.Client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxManagementResponseBytes+1))
	if err != nil {
		return response.StatusCode, nil, err
	}
	if int64(len(body)) > maxManagementResponseBytes {
		return response.StatusCode, nil, fmt.Errorf("management response exceeds %d bytes", maxManagementResponseBytes)
	}
	return response.StatusCode, body, nil
}

func (c *HTTPDeploymentClient) readyObservation(deployment apiDeployment) (DeploymentObservation, bool) {
	if deployment.RuntimeState != "running" || len(deployment.Instances) == 0 {
		return DeploymentObservation{}, false
	}
	modelReady := len(deployment.ModelProgress) > 0
	for _, progress := range deployment.ModelProgress {
		if !modelFileReady(progress.State) {
			modelReady = false
		}
	}
	var leader *apiInstance
	for index := range deployment.Instances {
		instance := &deployment.Instances[index]
		if instance.State != "running" || instance.ContainerID == "" || instance.ImageDigestID == "" {
			return DeploymentObservation{}, false
		}
		if instance.ModelProgress != nil && modelFileReady(instance.ModelProgress.State) {
			modelReady = true
		}
		if leader == nil || (!instance.Headless && instance.NodeRank == 0) {
			leader = instance
		}
	}
	if leader == nil || !modelReady {
		return DeploymentObservation{}, false
	}
	argv := append([]string(nil), leader.Entrypoint...)
	argv = append(argv, leader.Argv...)
	environment := make(map[string]string, len(leader.Env))
	for key, value := range leader.Env {
		if sensitiveName(key) {
			environment[key] = "<redacted>"
		} else {
			environment[key] = value
		}
	}
	return DeploymentObservation{
		DeploymentID: deployment.ID, BaseURL: c.BaseURL, Model: deployment.ServedName,
		ImageStarted: true, WeightsLoaded: true, Healthy: true,
		ImageDigest: leader.ImageDigestID, EnvironmentMap: environment, EngineArgv: redactArgv(argv),
	}, true
}

func modelFileReady(state string) bool {
	return state == "present" || state == "registered"
}

func failedDeployment(deployment apiDeployment) (string, string, bool) {
	for _, instance := range deployment.Instances {
		if instance.State != "failed" {
			continue
		}
		stage := strings.TrimSpace(instance.Phase)
		if stage == "" {
			stage = "deployment_runtime"
		}
		message := strings.TrimSpace(instance.Message)
		if message == "" {
			message = "instance entered failed state"
		}
		return stage, message, true
	}
	if deployment.RuntimeState == "failed" {
		return "deployment_runtime", "deployment entered failed state", true
	}
	return "", "", false
}

func deploymentPhase(deployment apiDeployment) string {
	for _, instance := range deployment.Instances {
		if strings.TrimSpace(instance.Phase) != "" {
			return instance.Phase
		}
	}
	return ""
}

func managementStatusError(status int, body []byte) error {
	var response struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &response) == nil && strings.TrimSpace(response.Error) != "" {
		return fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(response.Error))
	}
	return fmt.Errorf("HTTP %d", status)
}

func redactArgv(argv []string) []string {
	out := append([]string(nil), argv...)
	for index := 0; index < len(out); index++ {
		token := out[index]
		name := token
		if split := strings.IndexByte(token, '='); split >= 0 {
			name = token[:split]
			if sensitiveName(name) {
				out[index] = name + "=<redacted>"
			}
			continue
		}
		if sensitiveName(name) && index+1 < len(out) {
			out[index+1] = "<redacted>"
			index++
		}
	}
	return out
}

func sensitiveName(name string) bool {
	normalized := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
	for _, marker := range []string{"TOKEN", "API_KEY", "ACCESS_KEY", "PRIVATE_KEY", "SECRET", "PASSWORD", "CREDENTIAL", "AUTHORIZATION"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}
