package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
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
	addDeployment(t, st, "m", store.DeployActive, false) // the model the lines of twoLines name
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

// Without a model the lines name their own: the console refuses a file with a
// model no active deployment serves (a stopped one included) as a product error
// that names the models and leaves no batch or file behind, warns about one
// whose sleep is held, and accepts a file of served models.
func TestBatchConsoleChecksTheModelsOfTheFile(t *testing.T) {
	st, console := modelStack(t, &scriptedSender{reply: func(string, []byte) batch.Reply {
		return batch.Reply{Status: 200, RequestID: "r", Body: []byte(`{"ok":1}`)}
	}})
	addDeployment(t, st, "serving", store.DeployActive, false)
	addDeployment(t, st, "napping", store.DeployActive, true)
	addDeployment(t, st, "stopped-one", store.DeployStopped, false)
	caller := api.BatchCaller{UserID: 2, All: true}
	file := func(models ...string) string {
		var b strings.Builder
		for i, m := range models {
			fmt.Fprintf(&b, `{"custom_id":"c%d","method":"POST","url":"/v1/chat/completions","body":{"model":%q,"messages":[]}}`+"\n", i, m)
		}
		return b.String()
	}
	create := func(fields map[string]string) (dto.Batch, error) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, v := range fields {
			if k != "file" {
				_ = mw.WriteField(k, v)
			}
		}
		w, _ := mw.CreateFormFile("file", "my.jsonl")
		_, _ = io.WriteString(w, fields["file"])
		_ = mw.Close()
		return console.CreateFromUpload(caller, multipart.NewReader(&buf, mw.Boundary()))
	}
	chat := "/v1/chat/completions"

	_, err := create(map[string]string{"file": file("serving", "Qwen/Qwen2.5-0.5B-Instruct-rp2l-clone", "stopped-one", "serving"), "endpoint": chat})
	be, ok := err.(*api.BatchError)
	if !ok || be.Status != 400 || be.Code != messages.ErrorBatchFileModelsUnknown {
		t.Fatalf("unknown models: %#v", err)
	}
	// The models go as a list for the client to join in its own language.
	if raw, _ := json.Marshal(be.Params); string(raw) != `{"models":["Qwen/Qwen2.5-0.5B-Instruct-rp2l-clone","stopped-one"],"more":0}` {
		t.Fatalf("unknown models params = %s", raw)
	}
	_, err = create(map[string]string{"file": file("g1", "g2", "g3", "g4", "g5", "g6", "g7"), "endpoint": chat})
	if be, ok := err.(*api.BatchError); !ok || be.Code != messages.ErrorBatchFileModelsUnknown {
		t.Fatalf("seven unknown models: %#v", err)
	} else if raw, _ := json.Marshal(be.Params); string(raw) != `{"models":["g1","g2","g3","g4","g5"],"more":2}` {
		t.Fatalf("seven unknown models params = %s", raw)
	}
	if page, err := console.List(caller, nil, 1, 50); err != nil || page.Total != 0 {
		t.Fatalf("a refused file left %d batches (%v)", page.Total, err)
	}
	var files int64
	if err := st.DB().Model(&store.BatchFile{}).Count(&files).Error; err != nil || files != 0 {
		t.Fatalf("a refused file left %d files (%v)", files, err)
	}

	ok1, err := create(map[string]string{"file": file("serving", "serving"), "endpoint": chat})
	if err != nil || ok1.Model != "" || len(ok1.Warnings) != 0 {
		t.Fatalf("served models: %+v err=%v", ok1, err)
	}
	held, err := create(map[string]string{"file": file("serving", "napping"), "endpoint": chat})
	if err != nil || len(held.Warnings) != 1 {
		t.Fatalf("held model: %+v err=%v", held, err)
	}
	if raw, _ := json.Marshal(held.Warnings); string(raw) != `[{"code":"BATCH_FILE_MODELS_SLEEP_HELD","params":{"models":["napping"],"more":0}}]` {
		t.Fatalf("held model warnings = %s", raw)
	}
	over, err := create(map[string]string{"file": file("ghost"), "endpoint": chat, "model": "serving"})
	if err != nil || over.Model != "serving" {
		t.Fatalf("an override replaces the unknown model: %+v err=%v", over, err)
	}
}

// The list a create dialog offers and the check a create makes judge every
// name alike: awake (or starting) serves, asleep with a sleep a request wakes
// is asleep, asleep with a held sleep - or on a deployment a request cannot
// wake - is held, and a shared name is as good as its best deployment.
func TestBatchModelsListAgreesWithLookup(t *testing.T) {
	st, console := modelStack(t, &scriptedSender{reply: func(string, []byte) batch.Reply { return batch.Reply{Status: 200} }})
	node, err := st.RegisterNode("n1", store.HashToken("token"), "n1")
	if err != nil {
		t.Fatal(err)
	}
	seq := 0
	deploy := func(served string, sleepable, held bool, levels ...int) {
		t.Helper()
		d, err := st.CreateDeployment(&store.Deployment{Name: fmt.Sprintf("%s-%d", served, seq), ServedName: fmt.Sprintf("%s-%d", served, seq), State: store.DeployActive, Sleepable: sleepable})
		if err != nil {
			t.Fatal(err)
		}
		// The store refuses a second active deployment of a name; a rollout's
		// pair is renamed after creation.
		if err := st.DB().Model(&store.Deployment{}).Where("id = ?", d.ID).Updates(map[string]any{"served_name": served, "sleep_held": held}).Error; err != nil {
			t.Fatal(err)
		}
		for i, level := range levels {
			if _, err := st.CreateInstance(&store.Instance{UUID: fmt.Sprintf("%s-%d-%d", served, seq, i), DeploymentID: d.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, SleepLevel: level}); err != nil {
				t.Fatal(err)
			}
		}
		seq++
	}
	deploy("awake", true, false, 0)
	deploy("starting", true, false)       // no running replica yet
	deploy("half", true, false, 1, 0)     // one replica serves
	deploy("napping", true, false, 1, 2)  // every replica asleep, a request wakes it
	deploy("ordered", true, true)         // a held sleep, whatever the instances report yet
	deploy("unwakeable", false, false, 1) // asleep, but not sleepable: a request does not wake it
	deploy("rolling", true, true)         // a held old deployment ...
	deploy("rolling", true, false, 1)     // ... beside a new one a request wakes
	addDeployment(t, st, "stopped-one", store.DeployStopped, false)

	m := batchModels{st}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []batch.ModelEntry{
		{Name: "awake", State: batch.ModelAvailable}, {Name: "half", State: batch.ModelAvailable}, {Name: "starting", State: batch.ModelAvailable},
		{Name: "napping", State: batch.ModelAsleep}, {Name: "rolling", State: batch.ModelAsleep},
		{Name: "ordered", State: batch.ModelSleepHeld}, {Name: "unwakeable", State: batch.ModelSleepHeld},
	}
	if fmt.Sprint(list) != fmt.Sprint(want) {
		t.Fatalf("list = %v\nwant %v", list, want)
	}
	for _, entry := range list {
		if got, err := m.Lookup(entry.Name); err != nil || got != entry.State {
			t.Errorf("Lookup(%s) = %v %v, the list says %v", entry.Name, got, err, entry.State)
		}
	}
	if got, _ := m.Lookup("stopped-one"); got != batch.ModelUnknown {
		t.Errorf("a stopped deployment's name = %v", got)
	}

	d, err := console.Dictionary()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d.Models)
	if wantJSON := `[{"name":"awake","state":"served"},{"name":"half","state":"served"},{"name":"starting","state":"served"},{"name":"napping","state":"asleep"},{"name":"rolling","state":"asleep"},{"name":"ordered","state":"asleep_held"},{"name":"unwakeable","state":"asleep_held"}]`; string(raw) != wantJSON {
		t.Fatalf("dictionary models = %s", raw)
	}
	if d.OutputRetentionDays != 30 || d.CompletionWindowHours != (dto.BatchWindowHours{Min: 1, Max: 72, Default: 24}) || d.MaxFileBytes != 200<<20 || d.MaxFileLines != 50000 || len(d.Statuses) != 8 || len(d.Endpoints) != 3 {
		t.Fatalf("dictionary = %+v", d)
	}
}
