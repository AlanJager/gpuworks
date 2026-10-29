//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestProcessABatchOnAnOfflineDeploymentSurvivesPreemption proves the Batch
// API across the real process boundary. 30 chat requests are uploaded as a
// JSONL file and run as a batch against a model served by an offline
// deployment. While the batch runs, a request for an online deployment on the
// same card takes the GPU: the arbiter puts the offline deployment to sleep, so
// the batch's requests are held (or retried) until the online deployment goes
// idle and the offline one wakes. The batch must still complete with 30 unique
// results and none failed. The engine is the fake engine in a real container;
// it does not cut an in-flight request with finish_reason "abort" on a sleep, so
// that classification is covered by the executor's unit tests, not here.
// Skipped without Docker.
//
//	go test -tags=multiproc -run TestProcessABatchOnAnOfflineDeploymentSurvivesPreemption -timeout 20m ./test/l2/
func TestProcessABatchOnAnOfflineDeploymentSurvivesPreemption(t *testing.T) {
	base, token, node, modelID := startDockerAutomationRig(t, "batchapi")

	create := func(served string, extra map[string]any) (uint, processInstance) {
		t.Helper()
		body := map[string]any{
			"model_id": modelID, "node_uuid": node, "served_name": served,
			"engine": "vllm", "image": "gpuworks/fake-engine:test", "sleepable": true,
		}
		for k, v := range extra {
			body[k] = v
		}
		raw, _ := json.Marshal(body)
		resp, data := authedDo(t, http.MethodPost, base, token, "/api/v1/deployments", raw)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s status=%d body=%s", served, resp.StatusCode, data)
		}
		var created struct {
			ID uint `json:"id"`
		}
		if err := json.Unmarshal(data, &created); err != nil {
			t.Fatal(err)
		}
		inst := waitInstance(t, base, token, func(inst processInstance) bool {
			return inst.DeploymentID == created.ID && inst.State == "running" && inst.ContainerID != "" && len(inst.Argv) > 0
		})
		t.Cleanup(func() {
			if resp, err := authedDo(t, http.MethodDelete, base, token, "/api/v1/deployments/"+itoa(created.ID), nil); err == nil && resp != nil {
				resp.Body.Close()
			}
			waitDeploymentGone(t, base, token, created.ID, 30*time.Second)
			_ = exec.Command("docker", "rm", "-f", inst.ContainerID).Run()
		})
		return created.ID, inst
	}
	put := func(path string, body any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		if resp, data := authedDo(t, http.MethodPut, base, token, path, raw); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s status=%d body=%s", path, resp.StatusCode, data)
		}
	}

	// The online deployment is put to sleep by idle sleep, which stays on: it
	// goes back to sleep a minute after the request that preempts the batch's
	// deployment, and that is when the offline deployment wakes again.
	online, onlineInst := create("batchapi-online", map[string]any{"service_level": "online"})
	put(fmt.Sprintf("/api/v1/deployments/%d/idle-sleep", online), map[string]any{"after_sec": 60, "level": 2})
	waitSleepLevelWithin(t, base, token, onlineInst.UUID, 2, 3*time.Minute)

	// The offline deployment takes the card the sleeper gave up. Five seconds
	// per request (four rounds of eight) keep the batch running while the
	// preemption happens.
	offline, _ := create("batchapi-offline", map[string]any{
		"service_level":       "offline",
		"container_overrides": map[string]any{"env": map[string]string{"FAKE_ENGINE_TTFT_DELAY": "5s"}},
	})

	_, keyData := authedDo(t, http.MethodPost, base, token, "/api/v1/apikeys", []byte(`{"name":"batchapi"}`))
	var key struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(keyData, &key); err != nil || key.Key == "" {
		t.Fatalf("api key: %s", keyData)
	}
	v1 := func(method, path, contentType string, body io.Reader) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key.Key)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data
	}

	const total = 30
	var jsonl strings.Builder
	for i := 0; i < total; i++ {
		fmt.Fprintf(&jsonl, `{"custom_id":"req-%02d","method":"POST","url":"/v1/chat/completions","body":{"model":"batchapi-offline","messages":[{"role":"user","content":"hello %d"}],"max_tokens":8}}`+"\n", i, i)
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	_ = mw.WriteField("purpose", "batch")
	part, _ := mw.CreateFormFile("file", "requests.jsonl")
	_, _ = io.WriteString(part, jsonl.String())
	_ = mw.Close()
	status, data := v1(http.MethodPost, "/v1/files", mw.FormDataContentType(), &form)
	var file struct {
		ID      string `json:"id"`
		Bytes   int    `json:"bytes"`
		Purpose string `json:"purpose"`
	}
	if err := json.Unmarshal(data, &file); err != nil || status != http.StatusOK || file.ID == "" || file.Purpose != "batch" || file.Bytes != jsonl.Len() {
		t.Fatalf("upload = %d %s", status, data)
	}

	status, data = v1(http.MethodPost, "/v1/batches", "application/json",
		strings.NewReader(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"suite":"l2"}}`, file.ID)))
	type batchObject struct {
		ID            string  `json:"id"`
		Status        string  `json:"status"`
		InProgressAt  *int64  `json:"in_progress_at"`
		CompletedAt   *int64  `json:"completed_at"`
		OutputFileID  *string `json:"output_file_id"`
		ErrorFileID   *string `json:"error_file_id"`
		RequestCounts struct {
			Total     int `json:"total"`
			Completed int `json:"completed"`
			Failed    int `json:"failed"`
		} `json:"request_counts"`
	}
	var batch batchObject
	if err := json.Unmarshal(data, &batch); err != nil || status != http.StatusOK || batch.ID == "" || batch.RequestCounts.Total != total {
		t.Fatalf("create batch = %d %s", status, data)
	}
	retrieve := func() batchObject {
		t.Helper()
		status, data := v1(http.MethodGet, "/v1/batches/"+batch.ID, "", nil)
		var got batchObject
		if err := json.Unmarshal(data, &got); err != nil || status != http.StatusOK {
			t.Fatalf("retrieve = %d %s", status, data)
		}
		return got
	}

	// Let the batch get going, then preempt it.
	deadline := time.Now().Add(2 * time.Minute)
	for retrieve().Status != "in_progress" {
		if time.Now().After(deadline) {
			t.Fatalf("the batch never started: %+v", retrieve())
		}
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	if got := retrieve(); got.Status != "in_progress" || got.RequestCounts.Completed >= total {
		t.Fatalf("the batch should still be running when the preemption starts: %+v", got)
	}

	preempt := time.Now()
	status, data = v1(http.MethodPost, "/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"batchapi-online","messages":[{"role":"user","content":"preempt"}]}`))
	if status != http.StatusOK {
		t.Fatalf("the request that preempts the batch = %d %s", status, data)
	}
	t.Logf("online request answered %v after it was sent", time.Since(preempt).Round(time.Millisecond))
	params := waitEvent(t, base, token, offline, "AUTO_SLEEP")
	if params["trigger"] != "preempt" || params["for_deployment"] != float64(online) {
		t.Fatalf("offline AUTO_SLEEP params = %v, want trigger preempt for %d", params, online)
	}

	// The batch finishes once the offline deployment is awake again.
	deadline = time.Now().Add(8 * time.Minute)
	var done batchObject
	for {
		done = retrieve()
		if done.Status == "completed" || done.Status == "failed" || done.Status == "expired" || done.Status == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the batch is still %s at %+v", done.Status, done.RequestCounts)
		}
		time.Sleep(2 * time.Second)
	}
	// The preemption must have landed while the batch was running, or the
	// test proves nothing.
	sleptAt := offlineSleepTime(t, base, token, offline)
	if done.InProgressAt == nil || done.CompletedAt == nil || sleptAt.Unix() < *done.InProgressAt || sleptAt.Unix() > *done.CompletedAt {
		t.Fatalf("the offline deployment was put to sleep at %v, outside the batch's run %v..%v", sleptAt, done.InProgressAt, done.CompletedAt)
	}
	if done.Status != "completed" || done.RequestCounts.Completed != total || done.RequestCounts.Failed != 0 || done.OutputFileID == nil || done.ErrorFileID != nil {
		t.Fatalf("batch = %+v", done)
	}

	status, data = v1(http.MethodGet, "/v1/files/"+*done.OutputFileID+"/content", "", nil)
	if status != http.StatusOK {
		t.Fatalf("output content = %d %s", status, data)
	}
	seen := map[string]bool{}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		var result struct {
			ID       string `json:"id"`
			CustomID string `json:"custom_id"`
			Response *struct {
				StatusCode int             `json:"status_code"`
				Body       json.RawMessage `json:"body"`
			} `json:"response"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			t.Fatalf("output line %q: %v", line, err)
		}
		if seen[result.CustomID] {
			t.Fatalf("custom_id %s appears twice in the output", result.CustomID)
		}
		seen[result.CustomID] = true
		if result.Response == nil || result.Response.StatusCode != http.StatusOK || string(result.Error) != "null" || !strings.HasPrefix(result.ID, "batch_req_") || !bytes.Contains(result.Response.Body, []byte(`"choices"`)) {
			t.Fatalf("output line = %s", line)
		}
	}
	if len(lines) != total || len(seen) != total {
		t.Fatalf("output has %d lines and %d unique custom_ids, want %d", len(lines), len(seen), total)
	}

	// The console sees the same batch.
	resp, data := authedDo(t, http.MethodGet, base, token, "/api/v1/batches/"+batch.ID, nil)
	var console struct {
		Status        string `json:"status"`
		InputFilename string `json:"input_filename"`
		RequestCounts struct {
			Completed int `json:"completed"`
		} `json:"request_counts"`
	}
	if err := json.Unmarshal(data, &console); err != nil || resp.StatusCode != http.StatusOK || console.Status != "completed" || console.InputFilename != "requests.jsonl" || console.RequestCounts.Completed != total {
		t.Fatalf("console batch = %d %s", resp.StatusCode, data)
	}
}

// offlineSleepTime is when the deployment's first preempting AUTO_SLEEP was
// recorded.
func offlineSleepTime(t *testing.T, base, token string, deploymentID uint) time.Time {
	t.Helper()
	_, data := authedDo(t, http.MethodGet, base, token, fmt.Sprintf("/api/v1/deployments/%d/events?limit=200", deploymentID), nil)
	var events struct {
		Items []struct {
			Code      string         `json:"code"`
			CreatedAt time.Time      `json:"created_at"`
			Params    map[string]any `json:"params"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatalf("events: %v (%s)", err, data)
	}
	var first time.Time
	for _, ev := range events.Items {
		if ev.Code == "AUTO_SLEEP" && ev.Params["trigger"] == "preempt" && (first.IsZero() || ev.CreatedAt.Before(first)) {
			first = ev.CreatedAt
		}
	}
	if first.IsZero() {
		t.Fatalf("no preempting AUTO_SLEEP among %s", data)
	}
	return first
}
