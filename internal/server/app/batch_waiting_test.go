package app

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A running batch whose model's deployment failed says why on its detail: the
// model, its state now, the gateway's last answer and how many requests wait.
// The list leaves it out; it empties once the batch ends.
func TestBatchConsoleSaysWhyRequestsWait(t *testing.T) {
	st, console := modelStack(t, &scriptedSender{reply: func(string, []byte) batch.Reply {
		return batch.Reply{Status: 503, Body: []byte(`{"error":{"message":"deployment for m failed to start; see the deployment list"}}`)}
	}})
	addDeployment(t, st, "m", store.DeployActive, false)
	// The deployment's replica fails after the batch was accepted.
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", "m-replica").Update("state", "failed").Error; err != nil {
		t.Fatal(err)
	}
	caller := api.BatchCaller{UserID: 2, All: true}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("endpoint", "/v1/chat/completions")
	w, _ := mw.CreateFormFile("file", "my.jsonl")
	_, _ = io.WriteString(w, twoLines)
	_ = mw.Close()
	created, err := console.CreateFromUpload(caller, multipart.NewReader(&buf, mw.Boundary()))
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(created.Warnings); string(raw) != `[{"code":"BATCH_FILE_MODELS_WAIT","params":{"models":["m"],"more":0,"state":"failed"}}]` {
		t.Fatalf("warnings = %s", raw)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := console.Get(caller, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Waiting) == 1 && got.Waiting[0].Requests == 2 {
			wait := got.Waiting[0]
			if wait.Model != "m" || wait.State != "failed" || wait.Status != 503 || wait.Detail != "deployment for m failed to start; see the deployment list" || wait.Since.IsZero() {
				t.Fatalf("waiting = %+v", wait)
			}
			raw, _ := json.Marshal(got)
			var wire map[string]any
			_ = json.Unmarshal(raw, &wire)
			entry, _ := wire["waiting"].([]any)
			if len(entry) != 1 {
				t.Fatalf("wire = %s", raw)
			}
			keys := map[string]bool{}
			for k := range entry[0].(map[string]any) {
				keys[k] = true
			}
			for _, k := range []string{"model", "state", "status", "detail", "since", "requests"} {
				if !keys[k] {
					t.Fatalf("wire waiting lacks %q: %s", k, raw)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting = %+v", got.Waiting)
		}
		time.Sleep(5 * time.Millisecond)
	}
	page, err := console.List(caller, nil, 1, 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].Waiting != nil {
		t.Fatalf("list = %+v err=%v", page.Items, err)
	}
	cancelled, err := console.Cancel(caller, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		got, _ := console.Get(caller, cancelled.ID)
		if got.Status == batch.StatusCancelled {
			if got.Waiting != nil {
				t.Fatalf("a cancelled batch still waits: %+v", got.Waiting)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %s", got.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
