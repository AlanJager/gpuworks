package store

import (
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestUsageFactsReadsTheFleetAndTheDay(t *testing.T) {
	s := newTestStore(t)
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	since, until, now := day, day.Add(24*time.Hour), day.Add(25*time.Hour)

	empty, err := s.UsageFacts(since, until, now)
	if err != nil || len(empty.Nodes) != 0 || len(empty.Deployments) != 0 || empty.SchemaVersion == 0 {
		t.Fatalf("empty = %+v, %v", empty, err)
	}

	node := func(uuid string, cols map[string]any) uint {
		n, err := s.RegisterNode(uuid, HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.db.Model(&Node{}).Where("id = ?", n.ID).Updates(cols).Error; err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	sshNode := node("u-ssh", map[string]any{"os": "linux", "kernel": "6.8.0-138-generic", "arch": "amd64", "gpu_vendor": "nvidia",
		"framework": "cuda", "framework_version": "12.4", "runtime_driver": "docker", "state": NodeStateOnline, "install_dir": "/opt/gpuworks"})
	if err := s.db.Create(&NodeSSHCredential{NodeID: sshNode, Host: "10.0.0.1", Port: 22}).Error; err != nil {
		t.Fatal(err)
	}
	cloudNode := node("u-cloud", map[string]any{"os": "linux", "state": NodeStateOffline})
	if err := s.db.Create(&Provider{Name: "p", Kind: "runpod"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&ProviderInstance{ProviderID: 1, ExternalID: "x", NodeID: cloudNode}).Error; err != nil {
		t.Fatal(err)
	}
	// install_dir without a credential row is not an SSH install.
	node("u-agent", map[string]any{"install_dir": "/opt/x"})
	for _, g := range []GPU{{NodeID: sshNode, Index: 0, Ident: "a", Name: "H20"}, {NodeID: sshNode, Index: 1, Ident: "b", Name: "H20"},
		{NodeID: sshNode, Index: 2, Ident: "c", Name: "H20", Ownership: "passthrough", HealthCode: "XID_79"}} {
		if err := s.db.Create(&g).Error; err != nil {
			t.Fatal(err)
		}
	}

	model := Model{Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Name: "q"}
	if err := s.db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	tpl := InferenceModel{ModelID: "qwen/qwen3-8b", Source: "upstream"}
	if err := s.db.Create(&tpl).Error; err != nil {
		t.Fatal(err)
	}
	dep := Deployment{Name: "d1", ModelID: model.ID, TemplateID: tpl.ID, TemplateSpecSource: "upstream", Engine: "vllm",
		State: DeployActive, NNodes: 2, Sleepable: true, PartitionBackend: "nvidia-dgpu", CreatedAt: day.Add(time.Hour)}
	if err := s.db.Create(&dep).Error; err != nil {
		t.Fatal(err)
	}
	insts := []Instance{{UUID: "i-ready", DeploymentID: dep.ID, State: proto.InstRunning, Role: "prefill"},
		{UUID: "i-failed", DeploymentID: dep.ID, State: proto.InstFailed}}
	for i := range insts {
		if err := s.db.Create(&insts[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	event := func(uuid, code, data string, at time.Time) {
		if err := s.db.Create(&InstanceEvent{InstanceUUID: uuid, DeploymentID: dep.ID, Kind: "state", Code: code, Data: data, CreatedAt: at}).Error; err != nil {
			t.Fatal(err)
		}
	}
	event("i-ready", instanceEventCreated, `{"state":"pending"}`, day.Add(2*time.Hour))
	event("i-ready", instanceEventStatusUpdated, `{"state":"running","phase":""}`, day.Add(2*time.Hour+90*time.Second))
	event("i-failed", instanceEventCreated, `{"state":"pending"}`, day.Add(23*time.Hour))
	event("i-failed", instanceEventStateChanged, `{"state":"failed"}`, until.Add(10*time.Minute)) // after the day, before now
	event("i-old", instanceEventCreated, `{"state":"pending"}`, day.Add(-time.Hour))              // before the day
	event("i-old", EventUnitRetryExhausted, `{}`, day.Add(3*time.Hour))
	event("i-late", instanceEventCreated, `{}`, now.Add(time.Minute)) // after now: not read

	for _, mf := range []ModelFile{
		{ModelID: model.ID, NodeID: sshNode, State: proto.MFPresent, Acquisition: "download", SizeBytes: 16 << 30, CreatedAt: day.Add(time.Hour)},
		{ModelID: model.ID, NodeID: cloudNode, State: proto.MFFailed, Acquisition: "download", CreatedAt: day.Add(-48 * time.Hour)},
	} {
		if err := s.db.Create(&mf).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Create(&BenchmarkRun{Name: "b", Mode: "grid", State: "done", Dataset: `{"kind":"multiturn"}`, CreatedAt: day.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&SourceCredential{Host: "huggingface.co"}).Error; err != nil {
		t.Fatal(err)
	}

	got, err := s.UsageFacts(since, until, now)
	if err != nil {
		t.Fatal(err)
	}
	byUUID := map[string]UsageNode{}
	for i, n := range got.Nodes {
		byUUID[[]string{"u-ssh", "u-cloud", "u-agent"}[i]] = n
	}
	if n := byUUID["u-ssh"]; !n.SSH || n.Cloud || n.Kernel != "6.8.0-138-generic" || n.Framework != "cuda" || n.RuntimeDriver != "docker" {
		t.Fatalf("ssh node = %+v", n)
	}
	if n := byUUID["u-cloud"]; n.SSH || !n.Cloud {
		t.Fatalf("cloud node = %+v", n)
	}
	if n := byUUID["u-agent"]; n.SSH || n.Cloud {
		t.Fatalf("agent node = %+v", n)
	}
	cards := map[string]int{}
	for _, c := range got.Cards {
		cards[c.Name+"/"+c.Ownership+"/"+c.HealthCode] = c.Count
	}
	if cards["H20//"] != 2 || cards["H20/passthrough/XID_79"] != 1 {
		t.Fatalf("cards = %v", cards)
	}
	if len(got.Providers) != 1 || got.Providers[0] != (NameCount{"runpod", 1}) {
		t.Fatalf("providers = %v", got.Providers)
	}
	if len(got.Deployments) != 1 {
		t.Fatalf("deployments = %+v", got.Deployments)
	}
	d := got.Deployments[0]
	if d.Engine != "vllm" || d.State != string(DeployActive) || d.NNodes != 2 || !d.Sleepable || d.PartitionBackend != "nvidia-dgpu" ||
		d.TemplateSource != "upstream" || d.TemplateModel != "qwen/qwen3-8b" || d.ModelSource != "huggingface" || d.ModelSourceID != "Qwen/Qwen3-8B" {
		t.Fatalf("deployment = %+v", d)
	}
	if got.PDDeployments != 1 {
		t.Fatalf("pd = %d", got.PDDeployments)
	}
	if len(got.TokenHosts) != 1 || got.TokenHosts[0] != "huggingface.co" {
		t.Fatalf("token hosts = %v", got.TokenHosts)
	}
	events := map[string]int{}
	for _, e := range got.Events {
		events[e.Name] = e.Count
	}
	if events[instanceEventCreated] != 2 || events[EventUnitRetryExhausted] != 1 || events[instanceEventStateChanged] != 0 {
		t.Fatalf("events = %v (only the day's)", events)
	}
	var lifecycle []string
	for _, e := range got.Lifecycle {
		lifecycle = append(lifecycle, e.InstanceUUID+":"+e.State)
	}
	if strings.Join(lifecycle, ",") != "i-ready:pending,i-ready:running,i-failed:pending,i-failed:failed" {
		t.Fatalf("lifecycle = %v", lifecycle)
	}
	if !got.Lifecycle[0].Created || got.Lifecycle[1].Created {
		t.Fatalf("created flags = %+v", got.Lifecycle)
	}
	if len(got.ModelFiles) != 1 || got.ModelFiles[0].SizeBytes != 16<<30 || got.ModelFilesFailed != 1 {
		t.Fatalf("model files = %+v, failed %d", got.ModelFiles, got.ModelFilesFailed)
	}
	if len(got.Benchmarks) != 1 || got.Benchmarks[0] != (UsageBenchmark{Mode: "grid", DatasetKind: "multiturn", State: "done"}) {
		t.Fatalf("benchmarks = %+v", got.Benchmarks)
	}
}

// The timeline is the one table that grows with the fleet's history; the
// report reads it by its created_at index, never by a scan.
func TestUsageFactsReadsTheTimelineByIndex(t *testing.T) {
	s := newTestStore(t)
	since, until := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"EXPLAIN QUERY PLAN SELECT code AS name, COUNT(*) AS count FROM instance_events WHERE created_at >= ? AND created_at < ? GROUP BY code", []any{since, until}},
		{"EXPLAIN QUERY PLAN SELECT instance_uuid, code, data, created_at FROM instance_events WHERE created_at >= ? AND created_at < ? AND code IN ?", []any{since, until, usageLifecycleCodes}},
	} {
		var plan []struct {
			Detail string `gorm:"column:detail"`
		}
		if err := s.db.Raw(q.sql, q.args...).Scan(&plan).Error; err != nil {
			t.Fatal(err)
		}
		for _, step := range plan {
			if strings.HasPrefix(step.Detail, "SCAN instance_events") {
				t.Fatalf("%s\nplan step %q scans the timeline", q.sql, step.Detail)
			}
		}
	}
}
