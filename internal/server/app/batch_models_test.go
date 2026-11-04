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
	svc := batch.New(batchRepo{st}, send, batch.Config{Dir: t.TempDir(), Models: batchModels{st: st}, RetryBase: 10 * time.Millisecond, RetryCap: 20 * time.Millisecond, CheckpointEvery: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); svc.Wait() })
	return st, batchConsole{svc}
}

// chatModelOps is the API contract of a chat model: the operations a model
// row stores, which the gateway allows requests by.
const chatModelOps = `[{"type":"openai.chat","endpoint":"/v1/chat/completions"},{"type":"openai.completions","endpoint":"/v1/completions"}]`

// modelOf adds a model row with the operations (a JSON []inferenceapi.Operation).
func modelOf(t *testing.T, st *store.Store, name, operations string) uint {
	t.Helper()
	m := &store.Model{Source: "local", SourceID: name, Name: name, Operations: operations}
	if err := st.DB().Create(m).Error; err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// onlineNode registers a node that is connected: replicas on it serve.
func onlineNode(t *testing.T, st *store.Store, uuid string) *store.Node {
	t.Helper()
	node, err := st.RegisterNode(uuid, store.HashToken("token-"+uuid), uuid)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(uuid, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	return node
}

// addDeployment adds a deployment of the served name, of a chat model. An
// active one has a running replica on a connected node: awake, or asleep by an
// operator's held sleep when held. A stopped one has none.
func addDeployment(t *testing.T, st *store.Store, served string, state store.DeploymentState, held bool) {
	t.Helper()
	d, err := st.CreateDeployment(&store.Deployment{Name: served + "-dep", ServedName: served, State: state, Sleepable: true, ModelID: modelOf(t, st, served, chatModelOps)})
	if err != nil {
		t.Fatal(err)
	}
	if state != store.DeployActive {
		return
	}
	node := onlineNode(t, st, served+"-node")
	in := &store.Instance{UUID: served + "-replica", DeploymentID: d.ID, NodeID: node.ID, State: proto.InstRunning, NNodes: 1}
	if held {
		in.SleepLevel, in.SleepHeld = 2, true
	}
	if _, err := st.CreateInstance(in); err != nil {
		t.Fatal(err)
	}
}

// A served name is running when an active deployment's replica is awake, held
// when every such deployment's sleep is held, stopped when only a stopped
// deployment serves it, and unknown when none does.
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
	m := batchModels{st: st}
	for name, want := range map[string]batch.ModelState{
		"serving": batch.ModelRunning, "held-only": batch.ModelSleepHeld, "rolling": batch.ModelRunning,
		"stopped-one": batch.ModelStopped, "ghost": batch.ModelUnknown,
	} {
		if got, err := m.Lookup(name); err != nil || got.State != want {
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

	_, err = console.CreateFromUpload(caller, form(map[string]string{"file": twoLines, "endpoint": "/v1/chat/completions", "model": "ghost"}))
	if be, ok := err.(*api.BatchError); !ok || be.Status != 400 || be.Code != api.ErrorBatchModelUnknown || be.Params["model"] != "ghost" {
		t.Fatalf("ghost: err = %#v", err)
	}
	// A stopped model is accepted: its requests wait for a start, and the
	// warning says so with the state for the client to word.
	parked, err := console.CreateFromUpload(caller, form(map[string]string{"file": twoLines, "endpoint": "/v1/chat/completions", "model": "stopped-one"}))
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(parked.Warnings); string(raw) != `[{"code":"BATCH_MODEL_WAITS","params":{"model":"stopped-one","state":"stopped"}}]` {
		t.Fatalf("stopped model warnings = %s", raw)
	}
	// A chat model for an embeddings batch is refused, naming the endpoint.
	embedLine := `{"custom_id":"e","method":"POST","url":"/v1/embeddings","body":{"model":"m","input":"x"}}` + "\n"
	_, err = console.CreateFromUpload(caller, form(map[string]string{"file": embedLine, "endpoint": "/v1/embeddings", "model": "serving"}))
	if be, ok := err.(*api.BatchError); !ok || be.Status != 400 || be.Code != api.ErrorBatchModelEndpoint || be.Params["model"] != "serving" || be.Params["endpoint"] != "/v1/embeddings" {
		t.Fatalf("a chat model for embeddings: err = %#v", err)
	}
	page, err := console.List(caller, nil, 1, 50)
	if err != nil || page.Total != 5 {
		t.Fatalf("refused uploads left batches behind: %d %v", page.Total, err)
	}
	for _, item := range page.Items {
		if item.ID == created.ID && item.Model != "serving" {
			t.Fatalf("list row = %+v", item)
		}
	}
}

// Without a model the lines name their own: the console refuses a file with a
// model no deployment serves as a product error that names the models and
// leaves no batch or file behind, warns about one whose sleep is held or whose
// deployment is stopped, and accepts a file of served models.
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
	if raw, _ := json.Marshal(be.Params); string(raw) != `{"models":["Qwen/Qwen2.5-0.5B-Instruct-rp2l-clone"],"more":0}` {
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
	parked, err := create(map[string]string{"file": file("serving", "stopped-one", "napping"), "endpoint": chat})
	if err != nil {
		t.Fatalf("stopped model: err=%v", err)
	}
	if raw, _ := json.Marshal(parked.Warnings); string(raw) != `[{"code":"BATCH_FILE_MODELS_SLEEP_HELD","params":{"models":["napping"],"more":0}},{"code":"BATCH_FILE_MODELS_WAIT","params":{"models":["stopped-one"],"more":0,"state":"stopped"}}]` {
		t.Fatalf("stopped model warnings = %s", raw)
	}
	over, err := create(map[string]string{"file": file("ghost"), "endpoint": chat, "model": "serving"})
	if err != nil || over.Model != "serving" {
		t.Fatalf("an override replaces the unknown model: %+v err=%v", over, err)
	}
}

// The list a create dialog offers and the check a create makes judge every
// name alike, from the state the deployment list shows: awake serves (running),
// asleep with a sleep a request wakes is asleep, no replica yet is starting,
// asleep with a held sleep - or on a deployment a request cannot wake - is
// held, a replica on a disconnected node is offline, failed replicas are
// failed and a stopped deployment is stopped; a shared name is as good as its
// best deployment, and the endpoints are those its model's contract allows.
func TestBatchModelsListAgreesWithLookup(t *testing.T) {
	st, console := modelStack(t, &scriptedSender{reply: func(string, []byte) batch.Reply { return batch.Reply{Status: 200} }})
	node := onlineNode(t, st, "n1")
	far, err := st.RegisterNode("n2", store.HashToken("token-n2"), "n2")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState("n2", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	chatModel := modelOf(t, st, "chat-model", chatModelOps)
	embedModel := modelOf(t, st, "embed-model", `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]`)
	seq := 0
	// Each replica is its reported sleep level, with h when an operator's
	// sleep holds it, x when it failed, o when its node is disconnected.
	deploy := func(served string, sleepable bool, model uint, replicas ...string) {
		t.Helper()
		d, err := st.CreateDeployment(&store.Deployment{Name: fmt.Sprintf("%s-%d", served, seq), ServedName: fmt.Sprintf("%s-%d", served, seq), State: store.DeployActive, Sleepable: sleepable, ModelID: model})
		if err != nil {
			t.Fatal(err)
		}
		// The store refuses a second active deployment of a name; a rollout's
		// pair is renamed after creation.
		if err := st.DB().Model(&store.Deployment{}).Where("id = ?", d.ID).Update("served_name", served).Error; err != nil {
			t.Fatal(err)
		}
		for i, replica := range replicas {
			in := &store.Instance{UUID: fmt.Sprintf("%s-%d-%d", served, seq, i), DeploymentID: d.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1}
			in.SleepLevel, in.SleepHeld = int(replica[0]-'0'), strings.HasSuffix(replica, "h")
			if strings.HasSuffix(replica, "x") {
				in.State = proto.InstFailed
			}
			if strings.HasSuffix(replica, "o") {
				in.NodeID = far.ID
			}
			if _, err := st.CreateInstance(in); err != nil {
				t.Fatal(err)
			}
		}
		seq++
	}
	deploy("awake", true, chatModel, "0")
	deploy("starting", true, chatModel)             // no replica yet
	deploy("half", true, chatModel, "1", "0")       // one replica serves
	deploy("half-held", true, chatModel, "0", "2h") // one replica serves beside a held one
	deploy("napping", true, chatModel, "1", "2")    // every replica asleep, a request wakes it
	deploy("mixed", true, chatModel, "1h", "2")     // asleep, one held: a request wakes the other
	deploy("ordered", true, chatModel, "1h", "2h")  // every replica held asleep
	deploy("unwakeable", false, chatModel, "1")     // asleep, but not sleepable: a request does not wake it
	deploy("rolling", true, chatModel, "2h")        // a held old deployment ...
	deploy("rolling", true, chatModel, "1")         // ... beside a new one a request wakes
	deploy("far", true, chatModel, "0o")            // its only replica's node is disconnected
	deploy("crashed", true, chatModel, "0x")        // its only replica failed
	deploy("embedder", true, embedModel, "0")       // serves embeddings only
	deploy("revived", true, chatModel, "0x", "0")   // a failed attempt beside a running replacement
	addDeployment(t, st, "stopped-one", store.DeployStopped, false)

	m := batchModels{st: st}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range list {
		got = append(got, fmt.Sprintf("%s:%d", e.Name, e.State))
	}
	want := []string{}
	for _, row := range []struct {
		state batch.ModelState
		names []string
	}{
		{batch.ModelRunning, []string{"awake", "embedder", "half", "half-held", "revived"}},
		{batch.ModelAsleep, []string{"mixed", "napping", "rolling"}},
		{batch.ModelStarting, []string{"starting"}},
		{batch.ModelSleepHeld, []string{"ordered", "unwakeable"}},
		{batch.ModelOffline, []string{"far"}},
		{batch.ModelFailed, []string{"crashed"}},
		{batch.ModelStopped, []string{"stopped-one"}},
	} {
		for _, name := range row.names {
			want = append(want, fmt.Sprintf("%s:%d", name, row.state))
		}
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("list = %v\nwant %v", got, want)
	}
	for _, entry := range list {
		if got, err := m.Lookup(entry.Name); err != nil || fmt.Sprint(got) != fmt.Sprint(entry) {
			t.Errorf("Lookup(%s) = %v %v, the list says %v", entry.Name, got, err, entry)
		}
	}
	if got, _ := m.Lookup("ghost"); got.State != batch.ModelUnknown || got.Endpoints != nil {
		t.Errorf("an unknown name = %+v", got)
	}

	d, err := console.Dictionary()
	if err != nil {
		t.Fatal(err)
	}
	var wire []map[string]any
	allJSON, err := json.Marshal(d.Models)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(allJSON, &wire); err != nil {
		t.Fatal(err)
	}
	for _, model := range wire {
		state := model["state"]
		wantReady := state == "running" || state == "asleep"
		if ready, ok := model["ready"].(bool); !ok || ready != wantReady {
			t.Errorf("model %s readiness = %v, want %v", model["name"], model["ready"], wantReady)
		}
	}
	raw, _ := json.Marshal(d.Models[:2])
	if wantJSON := `[{"name":"awake","state":"running","ready":true,"endpoints":["/v1/chat/completions","/v1/completions"]},{"name":"embedder","state":"running","ready":true,"endpoints":["/v1/embeddings"]}]`; string(raw) != wantJSON {
		t.Fatalf("dictionary models = %s", raw)
	}
	var states []string
	for _, model := range d.Models {
		if len(states) == 0 || states[len(states)-1] != model.State {
			states = append(states, model.State)
		}
	}
	if strings.Join(states, ",") != "running,asleep,starting,asleep_held,offline,failed,stopped" {
		t.Fatalf("dictionary states in order = %v", states)
	}
	if d.OutputRetentionDays != 30 || d.CompletionWindowHours != (dto.BatchWindowHours{Min: 1, Max: 72, Default: 24}) || d.MaxFileBytes != 200<<20 || d.MaxFileLines != 50000 || len(d.Statuses) != 8 || len(d.Endpoints) != 3 {
		t.Fatalf("dictionary = %+v", d)
	}
}
