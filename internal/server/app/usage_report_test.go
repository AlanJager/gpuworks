package app

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
)

// usageEndpoint is an SLS WebTracking fixture that keeps what it was sent.
type usageEndpoint struct {
	mu      sync.Mutex
	status  int
	reports []usagereport.Report
}

func (e *usageEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.Method == http.MethodGet {
		// The probe before a send: answered like the real logstore, nothing kept.
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"errorCode":"ParameterInvalid","errorMessage":"parameter count is less than 2"}`)
		return
	}
	var body io.Reader = r.Body
	if r.Header.Get("x-log-compresstype") == "deflate" {
		zr, err := zlib.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body = zr
	} else if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body = zr
	}
	raw, _ := io.ReadAll(body)
	var group struct {
		Logs []map[string]string `json:"__logs__"`
	}
	var report usagereport.Report
	if json.Unmarshal(raw, &group) == nil && len(group.Logs) == 1 {
		_ = json.Unmarshal([]byte(group.Logs[0]["report"]), &report)
	}
	// Every attempt is kept, refused ones too.
	e.reports = append(e.reports, report)
	if e.status != 0 {
		w.WriteHeader(e.status)
		io.WriteString(w, `{"errorCode":"InternalServerError","errorMessage":"fixture"}`)
		return
	}
	io.WriteString(w, "{}")
}

func (e *usageEndpoint) sent() []usagereport.Report {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]usagereport.Report(nil), e.reports...)
}

type usageRig struct {
	store    *store.Store
	settings *serverSettings
	usage    *usageReporter
	endpoint *usageEndpoint
	now      time.Time
	config   string
}

func newUsageRig(t *testing.T) *usageRig {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	config := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(config, []byte("listen:\n  console: [\"127.0.0.1:8800\"]\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	settings := newServerSettings(Config{ConfigPath: config}, nil, nil)
	settings.installID = "0123456789abcdef"
	endpoint := &usageEndpoint{}
	srv := httptest.NewServer(endpoint)
	t.Cleanup(srv.Close)
	rig := &usageRig{store: st, settings: settings, endpoint: endpoint, config: config,
		now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	rig.usage = &usageReporter{store: st, settings: settings, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:    func() time.Time { return rig.now },
		sender: &usagereport.Sender{Endpoints: []usagereport.Endpoint{{Kind: usagereport.EndpointSLS, URL: srv.URL + "/logstores/reports/track"}}}}
	return rig
}

func TestUsageReportSendsAtOnceOnAFirstStartThenDaily(t *testing.T) {
	rig := newUsageRig(t)
	rig.now = time.Date(2026, 10, 22, 0, 10, 0, 0, time.UTC) // before the day's due time
	// A fresh installation reports at once, whatever the time of day.
	rig.usage.tick(context.Background())
	sent := rig.endpoint.sent()
	if len(sent) != 1 || sent[0].Day != "2026-10-21" || sent[0].InstallID != "0123456789abcdef" || sent[0].Schema != usagereport.Schema {
		t.Fatalf("first start sent %+v", sent)
	}
	// Later that day nothing more is due.
	if wait := rig.usage.tick(context.Background()); wait <= 0 || wait > usageIdle || len(rig.endpoint.sent()) != 1 {
		t.Fatalf("same-day tick waited %v and sent %d", wait, len(rig.endpoint.sent()))
	}
	// The next day, once due, the 22nd is reported, once, with its counts.
	rig.now = time.Date(2026, 10, 23, 7, 0, 0, 0, time.UTC) // past 00:30 + the six-hour spread
	rig.usage.Counters().Inc(usagereport.KindRoute, "GET /nodes")
	rig.usage.tick(context.Background())
	rig.usage.tick(context.Background())
	sent = rig.endpoint.sent()
	if len(sent) != 2 || sent[1].Day != "2026-10-22" {
		t.Fatalf("sent = %+v", sent)
	}
	if len(sent[1].Usage.Routes) != 1 || sent[1].Usage.Routes[0] != (usagereport.Count{Name: "GET /nodes", N: 1}) {
		t.Fatalf("routes = %v", sent[1].Usage.Routes)
	}
	status := rig.usage.Status()
	if !status.Enabled || status.LastDay != "2026-10-22" || status.LastAttempt == nil || !status.LastAttempt.OK || status.NextDueAt == nil ||
		!status.NextDueAt.After(time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("status = %+v", status)
	}
	if left := rig.usage.Counters().Peek(); len(left) != 0 {
		t.Fatalf("counts left after delivery: %v", left)
	}
}

// Switching the program back on sends a report at once: the scheduler is
// woken rather than left to its next idle check.
func TestUsageReportSwitchedBackOnSendsAtOnce(t *testing.T) {
	rig := newUsageRig(t)
	rig.usage.kick = make(chan struct{}, 1)
	rig.now = time.Date(2026, 10, 22, 0, 10, 0, 0, time.UTC)
	if err := rig.store.SetUsageReportLastDay("2026-10-21"); err != nil {
		t.Fatal(err)
	}
	if err := rig.usage.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); rig.usage.Run(ctx) }()
	// Off, and nothing due anyway: nothing is sent.
	if err := rig.usage.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(rig.endpoint.sent()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("switching the program on sent nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if sent := rig.endpoint.sent(); len(sent) != 1 || sent[0].Day != "2026-10-21" {
		t.Fatalf("sent = %+v", sent)
	}
	// Switching it on when it already is does not send again.
	if err := rig.usage.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if day, _ := rig.store.UsageReportLastDay(); day != "2026-10-21" {
		t.Fatalf("last day = %q", day)
	}
}

// A server started without server.yaml (a test, a developer's run) never
// reports, and GPUWORKS_USAGE_REPORT=off keeps an installation out too.
func TestUsageReportOnlyFromAnInstallation(t *testing.T) {
	rig := newUsageRig(t)
	rig.usage.settings = newServerSettings(Config{}, nil, nil)
	rig.usage.settings.installID = "0123456789abcdef"
	rig.now = time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC)
	rig.usage.tick(context.Background())
	if len(rig.endpoint.sent()) != 0 {
		t.Fatal("a server without server.yaml reported")
	}
	if status := rig.usage.Status(); status.Enabled || status.Editable {
		t.Fatalf("status = %+v", status)
	}
}

func TestUsageReportFailureIsNotRetriedAndKeepsItsCounts(t *testing.T) {
	rig := newUsageRig(t)
	if err := rig.store.SetUsageReportLastDay("2026-10-20"); err != nil {
		t.Fatal(err)
	}
	rig.endpoint.status = http.StatusInternalServerError
	rig.now = time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC)
	rig.usage.Counters().Inc(usagereport.KindPanic, "server/x.f")
	rig.usage.tick(context.Background())
	rig.usage.tick(context.Background())
	rig.usage.tick(context.Background())
	if n := len(rig.endpoint.sent()); n != 1 {
		t.Fatalf("a failed day was tried %d times", n)
	}
	status := rig.usage.Status()
	if status.LastAttempt == nil || status.LastAttempt.OK || status.LastAttempt.Day != "2026-10-21" || !strings.Contains(status.LastAttempt.Error, "500") {
		t.Fatalf("status = %+v", status.LastAttempt)
	}
	if day, _ := rig.store.UsageReportLastDay(); day != "2026-10-20" {
		t.Fatalf("a failed day was recorded as reported: %q", day)
	}
	// The next day carries the undelivered counts.
	rig.endpoint.status = 0
	rig.now = time.Date(2026, 10, 23, 7, 0, 0, 0, time.UTC)
	rig.usage.tick(context.Background())
	sent := rig.endpoint.sent()
	if len(sent) != 2 || sent[1].Day != "2026-10-22" || len(sent[1].Errors.Panics) != 1 {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestUsageReportOffSendsNothingAndForgets(t *testing.T) {
	rig := newUsageRig(t)
	if err := rig.store.SetUsageReportLastDay("2026-10-20"); err != nil {
		t.Fatal(err)
	}
	if err := rig.usage.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(rig.config)
	if !strings.Contains(string(raw), "usage_report: false") {
		t.Fatalf("server.yaml = %s", raw)
	}
	rig.now = time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC)
	rig.usage.Counters().Inc(usagereport.KindRoute, "GET /nodes")
	rig.usage.tick(context.Background())
	if len(rig.endpoint.sent()) != 0 || len(rig.usage.Counters().Peek()) != 0 {
		t.Fatalf("off: sent %d, counts %v", len(rig.endpoint.sent()), rig.usage.Counters().Peek())
	}
	if status := rig.usage.Status(); status.Enabled || status.NextDueAt != nil {
		t.Fatalf("status = %+v", status)
	}
	// Without server.yaml the switch has nowhere to be kept.
	readOnly := &usageReporter{store: rig.store, settings: newServerSettings(Config{}, nil, nil)}
	if err := readOnly.SetEnabled(false); err == nil {
		t.Fatal("switched without server.yaml")
	}
}

func TestUsageReportPreviewIsTheNextReportWithoutTakingIt(t *testing.T) {
	rig := newUsageRig(t)
	rig.now = time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC)
	rig.usage.Counters().Inc(usagereport.KindLogin, "ok")
	raw, err := rig.usage.Preview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var r usagereport.Report
	if err := json.Unmarshal(raw, &r); err != nil || r.Day != "2026-10-21" || len(r.Usage.Logins) != 1 {
		t.Fatalf("preview = %s, %v", raw, err)
	}
	if len(rig.usage.Counters().Peek()) != 1 {
		t.Fatal("the preview took the counts")
	}
	if len(rig.endpoint.sent()) != 0 {
		t.Fatal("the preview sent something")
	}
}

// What leaves the installation: enumerated values, versions cut to
// major.minor, public model IDs only, never a node name or address.
func TestUsageReportBuildNamesOnlyWhatIsPublic(t *testing.T) {
	rig := newUsageRig(t)
	st := rig.store
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	n, err := st.RegisterNode("secret-node-uuid", store.HashToken("t"), "secret-hostname")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{
		"os": "linux", "kernel": "5.15.0-91-secretbuild", "arch": "amd64", "gpu_vendor": "nvidia", "gpu_driver": "550.54.15",
		"framework": "cuda", "framework_version": "12.4.1", "runtime_driver": "docker", "state": store.NodeStateOnline,
		"agent_version": "v1.2.3", "last_remote_addr": "10.1.2.3:4567",
	}).Error; err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"NVIDIA H20", "NVIDIA H20"} {
		if err := st.DB().Create(&store.GPU{NodeID: n.ID, Index: i, Ident: name + string(rune('a'+i)), Name: name}).Error; err != nil {
			t.Fatal(err)
		}
	}
	public := store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Name: "q"}
	private := store.Model{Source: "modelscope", SourceID: "acme/secret-finetune", Name: "s"}
	local := store.Model{Source: "local", SourceID: "/data/models/secret", Name: "l"}
	for _, m := range []*store.Model{&public, &private, &local} {
		if err := st.DB().Create(m).Error; err != nil {
			t.Fatal(err)
		}
	}
	// A credential for ModelScope: its repositories may be private.
	if err := st.DB().Create(&store.SourceCredential{Host: "modelscope.cn"}).Error; err != nil {
		t.Fatal(err)
	}
	for i, m := range []store.Model{public, private, local} {
		d := store.Deployment{Name: "secret-deployment-" + string(rune('a'+i)), ModelID: m.ID, Engine: "vllm", State: store.DeployActive, CreatedAt: day.Add(time.Hour)}
		if err := st.DB().Create(&d).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DB().Create(&store.InstanceEvent{InstanceUUID: "i1", Kind: "state", Code: "INSTANCE_CREATED", Data: `{"state":"pending"}`, CreatedAt: day.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.InstanceEvent{InstanceUUID: "i1", Kind: "state", Code: "INSTANCE_STATUS_UPDATED", Data: `{"state":"` + proto.InstRunning + `"}`, CreatedAt: day.Add(time.Hour + 5*time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	r, err := rig.usage.build(context.Background(), day, day.Add(30*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := usagereport.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret", "10.1.2.3", "/data/models"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("the report carries %q: %s", secret, raw)
		}
	}
	models := map[string]int{}
	for _, c := range r.Usage.Models {
		models[c.Name] = c.N
	}
	if models["huggingface:Qwen/Qwen3-8B"] != 1 || models["custom"] != 2 {
		t.Fatalf("models = %v", models)
	}
	e := r.Environment
	if e.Nodes != 1 || e.OnlineNodes != 1 || e.Kernels[0].Name != "5.15" || e.Frameworks[0].Name != "cuda 12.4" ||
		e.Drivers[0].Name != "nvidia 550" || e.GPUModels[0] != (usagereport.Count{Name: "NVIDIA H20", N: 2}) || e.Onboarding[0].Name != "agent" {
		t.Fatalf("environment = %+v", e)
	}
	if r.Tasks.InstancesStarted != 1 || r.Tasks.InstancesReady != 1 || r.Tasks.ReadySeconds[0].Name != "2m-10m" || len(r.Tasks.DeploymentsCreated) != 1 {
		t.Fatalf("tasks = %+v", r.Tasks)
	}
	if r.Usage.Deployments != 3 || r.Usage.TemplateSources[0] != (usagereport.Count{Name: "none", N: 3}) {
		t.Fatalf("usage = %+v", r.Usage)
	}
}
