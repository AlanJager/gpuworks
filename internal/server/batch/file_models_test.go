package batch

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// countedModels answers like the platform's lookup (a name it does not list is
// unknown, one in failing is a store failure) and counts the lookups per name.
type countedModels struct {
	fakeModels
	mu    sync.Mutex
	calls map[string]int
}

func newCountedModels(states map[string]ModelState) *countedModels {
	return &countedModels{fakeModels: fakeModels{states: states, failing: map[string]bool{"broken": true}}, calls: map[string]int{}}
}

func (c *countedModels) Lookup(name string) (ModelState, error) {
	c.mu.Lock()
	c.calls[name]++
	c.mu.Unlock()
	return c.fakeModels.Lookup(name)
}

func (c *countedModels) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

// modelLines is one chat request per model name, in order.
func modelLines(models ...string) string {
	var b strings.Builder
	for i, m := range models {
		fmt.Fprintf(&b, `{"custom_id":"c%d","method":"POST","url":"/v1/chat/completions","body":{"model":%q,"messages":[]}}`+"\n", i, m)
	}
	return b.String()
}

func fileModelsRig(t *testing.T, models *countedModels) *rig {
	t.Helper()
	r := newRig(t, Config{Models: models}, echo)
	r.start()
	return r
}

func (r *rig) createOver(content, model string) (Batch, File, error) {
	r.t.Helper()
	f, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(content))
	if err != nil {
		r.t.Fatalf("upload: %v", err)
	}
	b, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "24h", Model: model})
	return b, f, err
}

var servedModels = map[string]ModelState{"real": ModelAvailable, "real-2": ModelAvailable, "held": ModelSleepHeld, "held-2": ModelSleepHeld}

// A file naming a model no active deployment serves is refused when the batch
// is created: the models are named, no batch is created and the file stays.
func TestCreateBatchRefusesAFileOfUnservedModels(t *testing.T) {
	models := newCountedModels(servedModels)
	r := fileModelsRig(t, models)

	b, f, err := r.createOver(modelLines("real", "Qwen/Qwen2.5-0.5B-Instruct-rp2l-clone", "held", "ghost", "real", "ghost"), "")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("batch = %+v err = %v, want a refusal", b, err)
	}
	if e.Status != 400 || e.Type != "invalid_request_error" || e.Code != "model_not_found" || e.Param != "input_file_id" {
		t.Fatalf("error = %+v", e)
	}
	if got := strings.Join(e.Models, "|"); got != "Qwen/Qwen2.5-0.5B-Instruct-rp2l-clone|ghost" {
		t.Fatalf("models = %q", got)
	}
	for _, name := range e.Models {
		if !strings.Contains(e.Message, name) {
			t.Errorf("message does not name %q: %s", name, e.Message)
		}
	}
	if !strings.Contains(e.Message, "Set a model") || !strings.Contains(e.Message, "deploy") {
		t.Errorf("message does not say what to do: %s", e.Message)
	}
	if len(r.repo.batches) != 0 || r.repo.created != 0 {
		t.Fatalf("a refused file left %d batches", len(r.repo.batches))
	}
	if _, err := r.repo.GetFile(f.ID); err != nil {
		t.Fatalf("the file is gone from the repo: %v", err)
	}
	if path, _ := r.svc.filePath(f.ID); path == "" {
		t.Fatal("no file path")
	} else if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file's bytes are gone: %v", err)
	}
	// The file is still good for a batch that names a model or whose models are served now.
	if _, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "24h", Model: "real"}); err != nil {
		t.Fatalf("the refused file with an override: %v", err)
	}
}

func TestCreateBatchAcceptsAFileOfServedModels(t *testing.T) {
	models := newCountedModels(servedModels)
	r := fileModelsRig(t, models)
	b, _, err := r.createOver(modelLines("real", "real-2", "real"), "")
	if err != nil || b.Model != "" || len(b.Warnings) != 0 || b.Total != 3 {
		t.Fatalf("batch = %+v err = %v", b, err)
	}
	if models.calls["real"] != 1 || models.calls["real-2"] != 1 || models.total() != 2 {
		t.Fatalf("lookups = %v", models.calls)
	}
}

// An embeddings file names its models the same way.
func TestCreateBatchChecksTheModelsOfAnEmbeddingsFile(t *testing.T) {
	r := fileModelsRig(t, newCountedModels(servedModels))
	content := `{"custom_id":"e1","method":"POST","url":"/v1/embeddings","body":{"model":"ghost-embed","input":"x"}}` + "\n"
	f, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointEmbedding, CompletionWindow: "24h"})
	var e *Error
	if !errors.As(err, &e) || e.Code != "model_not_found" || len(e.Models) != 1 || e.Models[0] != "ghost-embed" {
		t.Fatalf("err = %v", err)
	}
}

// An override replaces the model of every line, so the file's models are not
// looked at; the override is checked as before.
func TestCreateBatchWithAnOverrideDoesNotCheckTheFilesModels(t *testing.T) {
	models := newCountedModels(servedModels)
	r := fileModelsRig(t, models)
	b, _, err := r.createOver(modelLines("ghost", "ghost-2"), "real")
	if err != nil || b.Model != "real" || len(b.Warnings) != 0 {
		t.Fatalf("batch = %+v err = %v", b, err)
	}
	if models.total() != 1 || models.calls["real"] != 1 {
		t.Fatalf("only the override is looked up: %v", models.calls)
	}
	_, _, err = r.createOver(modelLines("real"), "ghost")
	var e *Error
	if !errors.As(err, &e) || e.Code != "model_not_found" || e.Param != "model" || e.Model != "ghost" || len(e.Models) != 0 {
		t.Fatalf("an unknown override: %#v", err)
	}
}

// Models whose deployments sleep by an operator's or a schedule's order are
// accepted with the warning, as an override naming one is.
func TestCreateBatchWarnsAboutHeldModelsOfTheFile(t *testing.T) {
	r := fileModelsRig(t, newCountedModels(servedModels))
	b, _, err := r.createOver(modelLines("real", "held", "held-2", "held"), "")
	if err != nil || len(b.Warnings) != 1 {
		t.Fatalf("batch = %+v err = %v", b, err)
	}
	w := b.Warnings[0]
	if w.Code != WarningModelSleepHeld || w.Param != "input_file_id" || strings.Join(w.Models, "|") != "held|held-2" || !strings.Contains(w.Message, "held, held-2") {
		t.Fatalf("warning = %+v", w)
	}
	if stored, _ := r.repo.GetBatch(b.ID); len(stored.Warnings) != 0 {
		t.Fatalf("a warning was stored: %+v", stored.Warnings)
	}
	// A held model beside an unknown one is still a refusal.
	_, _, err = r.createOver(modelLines("held", "ghost"), "")
	var e *Error
	if !errors.As(err, &e) || e.Code != "model_not_found" {
		t.Fatalf("held and unknown: %v", err)
	}
}

// A message names a few models; a file of made-up names costs a bounded
// number of lookups.
func TestCreateBatchNamesAFewUnknownModelsAndStopsLookingUp(t *testing.T) {
	models := newCountedModels(servedModels)
	r := fileModelsRig(t, models)
	names := make([]string, 100)
	for i := range names {
		names[i] = fmt.Sprintf("ghost-%03d", i)
	}
	_, _, err := r.createOver(modelLines(names...), "")
	var e *Error
	if !errors.As(err, &e) || e.Code != "model_not_found" {
		t.Fatalf("err = %v", err)
	}
	if want := "ghost-000, ghost-001, ghost-002, ghost-003, ghost-004, ..."; !strings.Contains(e.Message, want) || strings.Contains(e.Message, "ghost-005") {
		t.Fatalf("message = %s", e.Message)
	}
	if len(e.Models) != maxReportedModels || models.total() != maxReportedModels {
		t.Fatalf("collected %d models after %d lookups, want %d", len(e.Models), models.total(), maxReportedModels)
	}
}

// A file at the line limit with a few models is scanned once and each model is
// looked up once, not once per line.
func TestCreateBatchLooksEachModelOfALargeFileUpOnce(t *testing.T) {
	models := newCountedModels(servedModels)
	r := fileModelsRig(t, models)
	pool := []string{"real", "real-2", "held"}
	names := make([]string, MaxFileLines)
	for i := range names {
		names[i] = pool[i%len(pool)]
	}
	b, _, err := r.createOver(modelLines(names...), "")
	if err != nil || b.Total != MaxFileLines || len(b.Warnings) != 1 {
		t.Fatalf("batch = %+v err = %v", b, err)
	}
	if models.total() != 3 || models.calls["real"] != 1 || models.calls["real-2"] != 1 || models.calls["held"] != 1 {
		t.Fatalf("lookups = %v", models.calls)
	}
}

func TestCreateBatchFileModelsLookupFailureIsAServerError(t *testing.T) {
	r := fileModelsRig(t, newCountedModels(servedModels))
	_, _, err := r.createOver(modelLines("real", "broken"), "")
	if err == nil || errors.As(err, new(*Error)) {
		t.Fatalf("a lookup failure is a server error, not a refusal: %v", err)
	}
	if len(r.repo.batches) != 0 {
		t.Fatalf("a failed lookup left %d batches", len(r.repo.batches))
	}
}

func TestCreateBatchFileModelsOfAFileWhoseBytesAreGone(t *testing.T) {
	r := fileModelsRig(t, newCountedModels(servedModels))
	f, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(modelLines("real")))
	if err != nil {
		t.Fatal(err)
	}
	path, _ := r.svc.filePath(f.ID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err = r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "24h"})
	var e *Error
	if !errors.As(err, &e) || e.Status != 404 || e.Code != "file_not_found" {
		t.Fatalf("err = %v", err)
	}
}

// The /v1 wire: the OpenAI error object, naming the field, and the warning
// for held models on the create response alone.
func TestV1BatchRefusesAFileOfUnservedModels(t *testing.T) {
	v := &v1Rig{}
	r := newRig(t, Config{Models: newCountedModels(servedModels)}, echo)
	r.start()
	v.rig = r
	v.h = r.svc.V1Handler(func(*http.Request) (Owner, bool) { return testOwner, true })
	create := func(content string) *httptest.ResponseRecorder {
		file := v.uploadBatchFile("k", content)["id"].(string)
		return v.do("k", "POST", "/v1/batches", "application/json", strings.NewReader(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, file)))
	}

	rec := create(modelLines("real", "Qwen/Qwen2.5-0.5B-Instruct-rp2l-clone", "ghost"))
	want := `{"error":{"message":"The input file names models that no active deployment serves: Qwen/Qwen2.5-0.5B-Instruct-rp2l-clone, ghost. Set a model to send every request to, or deploy these models first","type":"invalid_request_error","param":"input_file_id","code":"model_not_found"}}`
	if rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("unknown: %d %s", rec.Code, rec.Body)
	}
	if list := v.do("k", "GET", "/v1/batches", "", nil); strings.Contains(list.Body.String(), `"id":"batch_`) {
		t.Fatalf("a refused file left a batch: %s", list.Body)
	}

	rec = create(modelLines("real", "held"))
	obj := decode(t, rec.Body.Bytes())
	if rec.Code != 200 || obj["model"] != nil {
		t.Fatalf("held: %d %s", rec.Code, rec.Body)
	}
	warnings, _ := obj["warnings"].([]any)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v", obj["warnings"])
	}
	w := warnings[0].(map[string]any)
	if w["code"] != "model_sleep_held" || w["param"] != "input_file_id" || !strings.Contains(w["message"].(string), "held") {
		t.Fatalf("warning = %v", w)
	}
}

// The console's form: the file is read as it is saved and the refusal leaves
// no file behind.
func TestConsoleFormRefusesAFileOfUnservedModels(t *testing.T) {
	r := fileModelsRig(t, newCountedModels(servedModels))
	form := func(fields map[string]string, content string) (Batch, error) {
		return r.svc.CreateFromUpload(testOwner, multipartForm(t, fields, content))
	}
	chat := map[string]string{"endpoint": EndpointChat}

	_, err := form(chat, modelLines("real", "ghost"))
	var e *Error
	if !errors.As(err, &e) || e.Code != "model_not_found" || e.Param != "input_file_id" || len(e.Models) != 1 || e.Models[0] != "ghost" {
		t.Fatalf("unknown: %v", err)
	}
	if len(r.repo.files) != 0 || len(r.repo.batches) != 0 {
		t.Fatalf("a refusal left %d files and %d batches", len(r.repo.files), len(r.repo.batches))
	}
	if names, _ := readDirNames(r.cfg.Dir); len(names) != 0 {
		t.Fatalf("a refusal left %v on disk", names)
	}
	if b, err := form(chat, modelLines("real")); err != nil || b.Total != 1 {
		t.Fatalf("served: %+v err=%v", b, err)
	}
	if b, err := form(map[string]string{"endpoint": EndpointChat, "model": "real"}, modelLines("ghost")); err != nil || b.Model != "real" {
		t.Fatalf("override: %+v err=%v", b, err)
	}
}
