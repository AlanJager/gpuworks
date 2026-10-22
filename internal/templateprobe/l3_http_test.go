package templateprobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPDeploymentClientCreateWaitAndNonForceCleanup(t *testing.T) {
	var polls atomic.Int32
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer management-jwt" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/deployments":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["model_id"] != float64(1) {
				t.Errorf("create body=%#v err=%v", body, err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":7,"served_name":"served-model","runtime_state":"pending"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/deployments/7":
			if deleted.Load() {
				http.NotFound(w, r)
				return
			}
			if polls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"id":7,"served_name":"served-model","runtime_state":"starting","instances":[{"state":"starting","phase":"container_launch"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{
				"id":7,"served_name":"served-model","runtime_state":"running",
				"model_progress":[{"state":"present"}],
				"instances":[{
					"state":"running","container_id":"container-1","image_digest_id":"sha256:abc",
					"node_rank":0,"headless":false,"argv":["vllm","serve","/model","--hf-token","argv-secret"],
					"env":{"OMP_NUM_THREADS":"1","HF_TOKEN":"env-secret"},"model_progress":{"state":"present"}
				}]
			}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/deployments/7":
			if r.URL.RawQuery != "" {
				t.Errorf("cleanup used query %q; force deletion is forbidden", r.URL.RawQuery)
			}
			deleted.Store(true)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"deleting"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewHTTPDeploymentClient(server.URL, "management-jwt", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.PollInterval = time.Millisecond
	handle, err := client.Create(context.Background(), json.RawMessage(`{"model_id":1}`))
	if err != nil || handle.ID != 7 {
		t.Fatalf("handle=%+v err=%v", handle, err)
	}
	observation, err := client.WaitReady(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	if !observation.ImageStarted || !observation.WeightsLoaded || !observation.Healthy ||
		observation.ImageDigest != "sha256:abc" || observation.Model != "served-model" ||
		len(observation.EngineArgv) != 5 || observation.EnvironmentMap["OMP_NUM_THREADS"] != "1" ||
		observation.EnvironmentMap["HF_TOKEN"] != "<redacted>" || observation.EngineArgv[4] != "<redacted>" {
		t.Fatalf("observation = %+v", observation)
	}
	if err := client.Cleanup(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if !deleted.Load() {
		t.Fatal("deployment was not deleted")
	}
}

func TestHTTPDeploymentClientRuntimeFailureIsPlatformFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":3,"runtime_state":"failed","instances":[{"state":"failed","phase":"health_check","message":"engine exited"}]}`))
	}))
	defer server.Close()
	client, err := NewHTTPDeploymentClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.WaitReady(context.Background(), DeploymentHandle{ID: 3})
	var platform PlatformStageError
	if !errors.As(err, &platform) || platform.Stage != "health_check" || !strings.Contains(platform.Error(), "engine exited") {
		t.Fatalf("error = %#v", err)
	}
}

func TestHTTPDeploymentClientRejectsMalformedManagementResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":`))
	}))
	defer server.Close()
	client, err := NewHTTPDeploymentClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Create(context.Background(), json.RawMessage(`{"model_id":1}`))
	var harness HarnessStageError
	if !errors.As(err, &harness) || harness.Stage != "deployment_create" {
		t.Fatalf("error = %#v", err)
	}
}
