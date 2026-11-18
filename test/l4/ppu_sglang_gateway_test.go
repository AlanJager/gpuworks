//go:build cloudgpu && partition

package l4

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestPPUSGLangGatewayStability(t *testing.T) {
	base := strings.TrimRight(os.Getenv("GPUWORKS_PPU_GATEWAY_URL"), "/")
	adminToken := os.Getenv("GPUWORKS_PPU_GATEWAY_ADMIN_TOKEN")
	fixture := os.Getenv("GPUWORKS_PPU_GATEWAY_DEPLOYMENT_ID")
	if base == "" || adminToken == "" || fixture == "" {
		skipOrFailExternal(t, "configure GPUWORKS_PPU_GATEWAY_URL, GPUWORKS_PPU_GATEWAY_ADMIN_TOKEN and GPUWORKS_PPU_GATEWAY_DEPLOYMENT_ID for a single-replica PPU SGLang chat deployment")
	}
	id, err := strconv.ParseUint(fixture, 10, 32)
	if err != nil || id == 0 {
		t.Fatal("GPUWORKS_PPU_GATEWAY_DEPLOYMENT_ID must be a positive deployment ID")
	}
	admin := &testenv.APIClient{Client: &http.Client{Timeout: 20 * time.Second}, BaseURL: base, Token: adminToken}
	path := fmt.Sprintf("/api/v1/deployments/%d", id)
	getDeployment := func(ctx context.Context) dto.Deployment {
		var deployment dto.Deployment
		if err := ppuGatewayJSON(ctx, admin, http.MethodGet, path, nil, http.StatusOK, &deployment); err != nil {
			t.Fatal(err)
		}
		if deployment.Engine != "sglang" || deployment.ServedName == "" {
			t.Fatalf("deployment %d is not a named SGLang fixture", id)
		}
		return deployment
	}

	startup, cancelStartup := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancelStartup()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var deployment dto.Deployment
	for {
		deployment = getDeployment(startup)
		if deployment.State == "running" && deployment.ReadyReplicas != nil && *deployment.ReadyReplicas == 1 {
			break
		}
		if deployment.State == "failed" || deployment.State == "stopped" {
			t.Fatalf("deployment %d became %s before gateway verification", id, deployment.State)
		}
		select {
		case <-startup.Done():
			t.Fatalf("deployment %d never reached one ready replica (last state %s)", id, deployment.State)
		case <-ticker.C:
		}
	}
	if len(deployment.Instances) != 1 || len(deployment.Instances[0].GPUIndexes) != 1 {
		t.Fatalf("deployment %d must have exactly one single-card replica", id)
	}
	instance := deployment.Instances[0]
	var node dto.Node
	if err := ppuGatewayJSON(t.Context(), admin, http.MethodGet, "/api/v1/nodes/"+instance.NodeUUID, nil, http.StatusOK, &node); err != nil {
		t.Fatal(err)
	}
	if node.GPUVendor != "ppu" {
		t.Fatalf("deployment %d ran on vendor %q, want ppu", id, node.GPUVendor)
	}
	if instance.Env["SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION"] != "0" {
		t.Fatal("SGLang liveness still queues generation requests behind inference")
	}

	var key struct {
		Key string `json:"key"`
	}
	if err := ppuGatewayJSON(t.Context(), admin, http.MethodPost, "/api/v1/apikeys", map[string]any{
		"name": "PPU SGLang gateway L4", "deployment_ids": []uint64{id}, "all_deployments": false,
	}, http.StatusCreated, &key); err != nil || key.Key == "" {
		t.Fatalf("create scoped gateway key: %v", err)
	}
	t.Cleanup(func() {
		if err := ppuGatewayJSON(context.Background(), admin, http.MethodDelete, "/api/v1/apikeys/"+auth.PrefixOf(key.Key), nil, http.StatusOK, nil); err != nil {
			t.Error(err)
		}
	})
	// A cold PPU chat can compile a kernel that the engine's built-in warmup
	// did not exercise. Require a real reply, then sample beyond the incident's
	// 90-second failure interval without accepting a replacement replica.
	gateway := &testenv.APIClient{Client: &http.Client{Timeout: 4 * time.Minute}, BaseURL: base, Token: key.Key}
	request := map[string]any{
		"model": deployment.ServedName, "max_tokens": 64,
		"messages":             []map[string]string{{"role": "user", "content": "What is 1 + 1? Answer briefly."}},
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
	}
	var observationDeadline time.Time
	for {
		var reply struct {
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		started := time.Now()
		if err := ppuGatewayJSON(t.Context(), gateway, http.MethodPost, "/v1/chat/completions", request, http.StatusOK, &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Model != deployment.ServedName || len(reply.Choices) != 1 || strings.TrimSpace(reply.Choices[0].Message.Content) == "" {
			t.Fatal("gateway did not return a non-empty completion for the requested PPU model")
		}
		t.Logf("PPU deployment %d gateway HTTP 200 in %s", id, time.Since(started).Round(time.Millisecond))
		current := getDeployment(t.Context())
		if current.State != "running" || current.ReadyReplicas == nil || *current.ReadyReplicas != 1 || len(current.Instances) != 1 || current.Instances[0].UUID != instance.UUID {
			t.Fatalf("deployment %d lost its original ready replica during the gateway test", id)
		}
		if observationDeadline.IsZero() {
			observationDeadline = time.Now().Add(90 * time.Second)
		} else if !started.Before(observationDeadline) {
			return
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(15 * time.Second):
		}
	}
}

func ppuGatewayJSON(ctx context.Context, client *testenv.APIClient, method, path string, payload any, status int, reply any) error {
	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, client.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, response.StatusCode, body)
	}
	if reply != nil {
		return json.NewDecoder(response.Body).Decode(reply)
	}
	return nil
}
