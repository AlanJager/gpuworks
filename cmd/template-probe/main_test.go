package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/templateprobe"
)

func TestRunL2CommandJSONContract(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"num_hidden_layers":32}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"l2-command", "--engine", "vllm", "--model", "/model path", "--config", configPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("run code = %d, stderr=%s", code, stderr.String())
	}
	var command templateprobe.L2Command
	if err := json.Unmarshal(stdout.Bytes(), &command); err != nil {
		t.Fatalf("decode output: %v; body=%s", err, stdout.String())
	}
	if command.Engine != "vllm" || !command.Truncation.Allowed || len(command.Argv) == 0 {
		t.Fatalf("command = %+v", command)
	}
}

func TestRunClassifyLeavesSemanticFailureInconclusive(t *testing.T) {
	stdin := bytes.NewBufferString("semantic_output_validation=FAIL output=!!!!!!!!")
	old := os.Stdin
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeEnd.Write(stdin.Bytes()); err != nil {
		t.Fatal(err)
	}
	_ = writeEnd.Close()
	os.Stdin = readEnd
	t.Cleanup(func() { os.Stdin = old; _ = readEnd.Close() })
	var stdout, stderr bytes.Buffer
	if code := run([]string{"classify", "--log", "-"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run code = %d, stderr=%s", code, stderr.String())
	}
	var match templateprobe.FailureMatch
	if err := json.Unmarshal(stdout.Bytes(), &match); err != nil {
		t.Fatal(err)
	}
	if match.Matched || match.Excludes {
		t.Fatalf("semantic failure classified as incompatible: %+v", match)
	}
}

func TestRunL3EndToEndJSONContractAndCleanup(t *testing.T) {
	const managementToken = "management-secret"
	const apiKey = "data-plane-secret"
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/deployments" && r.Method == http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer "+managementToken {
				t.Errorf("management authorization = %q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":41,"served_name":"l3-model","runtime_state":"pending"}`))
		case r.URL.Path == "/api/v1/deployments/41" && r.Method == http.MethodGet:
			if deleted.Load() {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{
				"id":41,"served_name":"l3-model","runtime_state":"running",
				"model_progress":[{"state":"present"}],
				"instances":[{"state":"running","container_id":"c41","image_digest_id":"sha256:l3",
				"argv":["vllm","serve","/model"],"env":{},"model_progress":{"state":"present"}}]
			}`))
		case r.URL.Path == "/api/v1/deployments/41" && r.Method == http.MethodDelete:
			if r.URL.RawQuery != "" {
				t.Errorf("delete query = %q", r.URL.RawQuery)
			}
			deleted.Store(true)
			w.WriteHeader(http.StatusAccepted)
		case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer "+apiKey {
				t.Errorf("data-plane authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"The answer is 4."}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	deploymentPath := filepath.Join(dir, "deployment.json")
	casePath := filepath.Join(dir, "case.json")
	if err := os.WriteFile(deploymentPath, []byte(`{"model_id":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(casePath, []byte(`{"case_id":"cli-l3","task":"text","prompt":"Return 4","expected_keywords":["4"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GPUWORKS_L3_MANAGEMENT_TOKEN_TEST", managementToken)
	t.Setenv("GPUWORKS_L3_API_KEY_TEST", apiKey)
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"l3", "--server", server.URL, "--deployment-json", deploymentPath, "--case-json", casePath,
		"--management-token-env", "GPUWORKS_L3_MANAGEMENT_TOKEN_TEST",
		"--api-key-env", "GPUWORKS_L3_API_KEY_TEST", "--poll-interval", "1ms",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var result templateprobe.L3Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v; body=%s", err, stdout.String())
	}
	if result.Status != templateprobe.L3StatusPass || !result.PlatformCompatible || !result.Cleanup.Completed || !deleted.Load() {
		t.Fatalf("result=%+v deleted=%v", result, deleted.Load())
	}
	if strings.Contains(stdout.String(), managementToken) || strings.Contains(stdout.String(), apiKey) {
		t.Fatal("L3 output leaked a secret")
	}
}
