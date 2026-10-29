package app

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func modelStack(t *testing.T, send batch.Sender) (*store.Store, batchConsole) {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := batch.New(batchRepo{st}, send, batch.Config{Dir: t.TempDir(), Models: batchModels{st}, RetryBase: 10 * time.Millisecond, RetryCap: 20 * time.Millisecond, CheckpointEvery: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); svc.Wait() })
	return st, batchConsole{svc}
}

func addDeployment(t *testing.T, st *store.Store, served string, state store.DeploymentState, held bool) {
	t.Helper()
	d, err := st.CreateDeployment(&store.Deployment{Name: served + "-dep", ServedName: served, State: state})
	if err != nil {
		t.Fatal(err)
	}
	if held {
		if err := st.DB().Model(&store.Deployment{}).Where("id = ?", d.ID).Update("sleep_held", true).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// A served name is available when an active deployment serves it, held when
// every such deployment's sleep is held, and unknown otherwise (a stopped
// deployment does not serve).
func TestBatchModelsLookup(t *testing.T) {
	st, _ := modelStack(t, &scriptedSender{reply: func(string, []byte) batch.Reply { return batch.Reply{Status: 200} }})
	addDeployment(t, st, "serving", store.DeployActive, false)
	addDeployment(t, st, "held-only", store.DeployActive, true)
	addDeployment(t, st, "rolling", store.DeployActive, true)
	// A replacement beside the old deployment of the same name: the store
	// refuses a second active one, so it is renamed after creation.
	addDeployment(t, st, "rolling-new", store.DeployActive, false)
	if err := st.DB().Model(&store.Deployment{}).Where("served_name = ?", "rolling-new").Update("served_name", "rolling").Error; err != nil {
		t.Fatal(err)
	}
	addDeployment(t, st, "stopped-one", store.DeployStopped, false)
	m := batchModels{st}
	for name, want := range map[string]batch.ModelState{
		"serving": batch.ModelAvailable, "held-only": batch.ModelSleepHeld, "rolling": batch.ModelAvailable,
		"stopped-one": batch.ModelUnknown, "ghost": batch.ModelUnknown,
	} {
		if got, err := m.Lookup(name); err != nil || got != want {
			t.Errorf("%s = %v err=%v, want %v", name, got, err, want)
		}
	}
}

// The console's create with a model: the store keeps it, the DTO names it, a
// held sleep warns, and a model nobody serves is a product error that leaves
// nothing behind.
func TestBatchConsoleModelThroughTheStore(t *testing.T) {
	sender := &scriptedSender{reply: func(_ string, body []byte) batch.Reply {
		return batch.Reply{Status: 200, RequestID: "r", Body: []byte(`{"ok":1}`)}
	}}
	st, console := modelStack(t, sender)
	addDeployment(t, st, "serving", store.DeployActive, false)
	addDeployment(t, st, "napping", store.DeployActive, true)
	addDeployment(t, st, "stopped-one", store.DeployStopped, false)
	caller := api.BatchCaller{UserID: 2, All: true}
	form := func(fields map[string]string) *multipart.Reader {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, v := range fields {
			if k == "file" {
				w, _ := mw.CreateFormFile("file", "my.jsonl")
				_, _ = io.WriteString(w, v)
			} else {
				_ = mw.WriteField(k, v)
			}
		}
		_ = mw.Close()
		return multipart.NewReader(&buf, mw.Boundary())
	}

	created, err := console.CreateFromUpload(caller, form(map[string]string{"file": twoLines, "endpoint": "/v1/chat/completions", "model": "serving"}))
	if err != nil || created.Model != "serving" || len(created.Warnings) != 0 {
		t.Fatalf("created = %+v err=%v", created, err)
	}
	row, err := st.GetBatch(created.ID)
	if err != nil || row.ModelOverride != "serving" {
		t.Fatalf("row = %+v err=%v", row, err)
	}
	got, err := console.Get(caller, created.ID)
	if err != nil || got.Model != "serving" || len(got.Warnings) != 0 {
		t.Fatalf("get = %+v err=%v", got, err)
	}
	// The requests of the batch go to the chosen model, not the file's.
	deadline := time.Now().Add(10 * time.Second)
	for {
		sender.mu.Lock()
		n := len(sender.sent)
		bodies := append([]string(nil), sender.sent...)
		sender.mu.Unlock()
		if n >= 2 {
			for _, body := range bodies {
				if !strings.Contains(body, `"model":"serving"`) || strings.Contains(body, `"model":"m"`) {
					t.Fatalf("a request was sent as %s", body)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sent %d requests", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	held, err := console.CreateFromUpload(caller, form(map[string]string{"file": twoLines, "endpoint": "/v1/chat/completions", "model": "napping"}))
	if err != nil || held.Model != "napping" || len(held.Warnings) != 1 || held.Warnings[0].Code != api.WarningBatchModelSleepHeld || held.Warnings[0].Params["model"] != "napping" {
		t.Fatalf("held = %+v err=%v", held, err)
	}
	again, _ := console.Get(caller, held.ID)
	if len(again.Warnings) != 0 || again.Model != "napping" {
		t.Fatalf("a later read = %+v", again)
	}

	timed, err := console.CreateFromUpload(caller, form(map[string]string{"file": twoLines, "endpoint": "/v1/chat/completions", "completion_window": "6h"}))
	if err != nil || timed.CompletionWindow != "6h" || timed.ExpiresAt.Sub(timed.CreatedAt) != 6*time.Hour {
		t.Fatalf("a 6h window = %+v err=%v", timed, err)
	}
	if row, _ := st.GetBatch(timed.ID); row == nil || row.CompletionWindow != "6h" || row.ExpiresAt.Sub(row.CreatedAt) != 6*time.Hour {
		t.Fatalf("stored 6h batch = %+v", row)
	}
	_, err = console.CreateFromUpload(caller, form(map[string]string{"file": twoLines, "endpoint": "/v1/chat/completions", "completion_window": "73h"}))
	if be, ok := err.(*api.BatchError); !ok || be.Code != api.ErrorBatchRequestInvalid || be.Params["field"] != "completion_window" || be.Params["reason"] != "invalid_completion_window" {
		t.Fatalf("a 73h window = %#v", err)
	}

	plain, err := console.CreateFromUpload(caller, form(map[string]string{"file": twoLines, "endpoint": "/v1/chat/completions"}))
	if err != nil || plain.Model != "" {
		t.Fatalf("plain = %+v err=%v", plain, err)
	}

	for _, name := range []string{"ghost", "stopped-one"} {
		_, err = console.CreateFromUpload(caller, form(map[string]string{"file": twoLines, "endpoint": "/v1/chat/completions", "model": name}))
		be, ok := err.(*api.BatchError)
		if !ok || be.Status != 400 || be.Code != api.ErrorBatchModelUnknown || be.Params["model"] != name {
			t.Fatalf("%s: err = %#v", name, err)
		}
	}
	page, err := console.List(caller, nil, 1, 50)
	if err != nil || page.Total != 4 {
		t.Fatalf("refused uploads left batches behind: %d %v", page.Total, err)
	}
	for _, item := range page.Items {
		if item.ID == created.ID && item.Model != "serving" {
			t.Fatalf("list row = %+v", item)
		}
	}
}
