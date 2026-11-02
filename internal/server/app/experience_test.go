package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/experiencelaunch"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// The environment sits beside the console: the console's addresses at its
// port plus ten, one address for everything under --addr, and the server's
// certificate only where TLS can apply (an explicit listen section).
func TestExperienceLaunchConfig(t *testing.T) {
	const bin = "/opt/gpuworks/bin/gpuworks"
	dir := t.TempDir()
	expDir := filepath.Join(dir, "experience")
	for _, tc := range []struct {
		name string
		cfg  Config
		want experiencelaunch.Config
		ok   bool
	}{
		{name: "addr", cfg: Config{DataDir: dir, Addr: ":8800", TLSCertFile: "c", TLSKeyFile: "k"},
			want: experiencelaunch.Config{Binary: bin, DataDir: expDir, Addr: ":8810", Port: 8810}, ok: true},
		{name: "addr on one IP", cfg: Config{DataDir: dir, Addr: "10.0.0.5:9000"},
			want: experiencelaunch.Config{Binary: bin, DataDir: expDir, Addr: "10.0.0.5:9010", Port: 9010}, ok: true},
		{name: "listen with TLS", cfg: Config{DataDir: dir, Listen: serverconfig.Listen{Console: []string{"10.0.0.5:8800"}, Gateway: []string{"10.0.1.5:8900"}},
			TLSCertFile: "/etc/tls.crt", TLSKeyFile: "/etc/tls.key"},
			want: experiencelaunch.Config{Binary: bin, DataDir: expDir, Listen: []string{"10.0.0.5:8810", "127.0.0.1:8810"}, Port: 8810,
				TLSCertFile: "/etc/tls.crt", TLSKeyFile: "/etc/tls.key"}, ok: true},
		{name: "inside the environment", cfg: Config{DataDir: dir, Addr: ":8800", Experience: &ExperienceConsole{}}},
		{name: "no data directory", cfg: Config{Addr: ":8800"}},
		{name: "port at the top", cfg: Config{DataDir: dir, Addr: ":65530"}},
	} {
		got, ok := experienceLaunchConfig(tc.cfg, bin)
		if ok != tc.ok || (ok && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("%s: got %+v, %v; want %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if experienceLauncher(Config{Addr: ":8800"}, nil) != nil {
		t.Fatal("a launcher without a data directory")
	}
}

// Only the environment's own console names its session cookie and guards
// its requests; a server leaves both as they are.
func TestExperienceConsoleWiring(t *testing.T) {
	plain := Config{}
	if plain.sessionCookie() != "" {
		t.Fatalf("server cookie = %q", plain.sessionCookie())
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	rec := httptest.NewRecorder()
	plain.experienceGuard(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("server guarded: %d", rec.Code)
	}

	guarded := Config{Experience: &ExperienceConsole{Guard: func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	}}}
	if guarded.sessionCookie() != experienceSessionCookie {
		t.Fatalf("environment cookie = %q", guarded.sessionCookie())
	}
	rec = httptest.NewRecorder()
	guarded.experienceGuard(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("environment not guarded: %d", rec.Code)
	}
}

// Seeded history lands where the live paths write theirs, and the days it
// covers are rolled up.
func TestSeedHistoryWritesAndRollsUp(t *testing.T) {
	now := time.Now()
	ops, st, obs := newTestOpsStats(t, &now)
	a := &App{ops: ops, store: st, obs: obs}
	ctx := context.Background()
	loc := ops.location()
	// The day before today's start: a Day is YYYYMMDD, so subtracting one
	// from the 1st of a month names a day that does not exist.
	todayStart, _ := rollup.DayOf(now, loc).Bounds(loc)
	yesterday := rollup.DayOf(time.UnixMilli(todayStart-1), loc)
	first, _ := yesterday.HoursOf(loc)
	hour := time.UnixMilli(first).Add(10 * time.Hour)
	key := rollup.GPUKey{HourMS: hour.UnixMilli(), NodeUUID: "n1", GPUIndex: 0, GPUUUID: "GPU-a"}
	named := key
	named.HourMS = 0
	h := History{
		Requests: []uxrecord.Request{finished(1, 1, hour.Add(5*time.Minute), 1000, 400)},
		Weights:  []int64{120},
		GPUs: map[rollup.GPUKey]rollup.GPU{key: {UtilSum: 60 * 120, UtilN: 120, UtilMax: 80, MemUsedSum: 120 * 70e9, MemUsedN: 120,
			MemUsedMax: 71e9, MemTotal: 80e9, PowerSum: 120 * 400, PowerN: 120, TempSum: 120 * 60, TempN: 120, TempMax: 64}},
		GPUModels: map[rollup.GPUKey]string{named: "H100 80GB HBM3"},
		GPUTime:   map[rollup.GPUTimeKey]rollup.GPUTime{{HourMS: key.HourMS, DeploymentID: 1, GPUModel: "H100 80GB HBM3"}: {GPUSeconds: 3600, WholeGPUSeconds: 3600, InstanceSeconds: 3600, MaxInstances: 1}},
		Capacity:  map[rollup.CapacityKey]rollup.Capacity{{HourMS: key.HourMS, GPUModel: "H100 80GB HBM3"}: {TotalGPUSeconds: 3600, SchedulableGPUSeconds: 3600}},
	}
	if err := a.SeedHistory(ctx, h); err != nil {
		t.Fatal(err)
	}
	rows := usageRows(t, st)
	if len(rows) != 1 || rows[0].Requests != 120 {
		t.Fatalf("usage rows = %+v", rows)
	}
	for _, kind := range []statsdb.Kind{statsdb.KindGPU, statsdb.KindGPUTime, statsdb.KindQuality} {
		if hourly, daily, err := ops.stats.RowCount(ctx, kind); err != nil || hourly == 0 || daily == 0 {
			t.Fatalf("%s rows = %d hourly, %d daily, %v", kind, hourly, daily, err)
		}
	}
	if rolled, err := st.UsageDayRolled(ctx, yesterday); err != nil || !rolled {
		t.Fatalf("usage day rolled = %v, %v", rolled, err)
	}
	if err := a.SeedHistory(ctx, History{Requests: h.Requests}); err == nil {
		t.Fatal("requests without weights were accepted")
	}
}

// The cards are the online nodes' own, with the deployments holding them and
// a dGPU slice's share; a card owned elsewhere is not the platform's.
func TestStatisticsCards(t *testing.T) {
	now := time.Now()
	ops, st, obs := newTestOpsStats(t, &now)
	a := &App{ops: ops, store: st, obs: obs}
	node, err := st.RegisterNode("n1", store.HashToken("n1"), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("state", store.NodeStateOnline).Error; err != nil {
		t.Fatal(err)
	}
	for _, g := range []store.GPU{
		{NodeID: node.ID, Ident: "GPU-a", UUID: "GPU-a", Name: "A100-SXM4-80GB", MemoryTotal: 80 << 30},
		{NodeID: node.ID, Ident: "GPU-b", UUID: "GPU-b", Index: 1, Name: "A100-SXM4-80GB", MemoryTotal: 80 << 30},
		{NodeID: node.ID, Ident: "GPU-c", UUID: "GPU-c", Index: 2, Name: "A100-SXM4-80GB", MemoryTotal: 80 << 30, Ownership: "passthrough"},
	} {
		g := g
		if err := st.DB().Create(&g).Error; err != nil {
			t.Fatal(err)
		}
	}
	whole, _ := st.CreateDeployment(&store.Deployment{Name: "whole", ServedName: "whole", State: store.DeployActive})
	slice, _ := st.CreateDeployment(&store.Deployment{Name: "slice", ServedName: "slice", State: store.DeployActive, VRAMLimitBytes: 20 << 30})
	for _, in := range []store.Instance{
		{UUID: "w", DeploymentID: whole.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", GPUIdents: `["GPU-a"]`},
		{UUID: "s", DeploymentID: slice.ID, NodeID: node.ID, State: "running", GPUIndexes: "[1]", GPUIdents: `["GPU-b"]`},
	} {
		in := in
		if _, err := st.CreateInstance(&in); err != nil {
			t.Fatal(err)
		}
	}
	cards, err := a.StatisticsCards()
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("cards = %+v", cards)
	}
	byUUID := map[string]StatisticsCard{}
	for _, c := range cards {
		byUUID[c.Key.GPUUUID] = c
	}
	if c := byUUID["GPU-a"]; c.Key.NodeUUID != node.UUID || c.Model != "A100-SXM4-80GB" || c.Holders[whole.ID] != 1 {
		t.Fatalf("whole card = %+v", c)
	}
	if c := byUUID["GPU-b"]; c.Key.GPUIndex != 1 || c.Holders[slice.ID] != 0.25 {
		t.Fatalf("sliced card = %+v", c)
	}
}

// Metric history goes into the raw store and reads back as a series.
func TestSeedMetricsReadsBack(t *testing.T) {
	now := time.Now()
	ops, st, obs := newTestOpsStats(t, &now)
	a := &App{ops: ops, store: st, obs: obs}
	at := now.Add(-time.Hour).UnixMilli()
	labels := map[string]string{"node": "n1", "gpu": "0"}
	if err := a.SeedMetrics([]tsdb.Sample{{Metric: "gpuworks_gpu_utilization", Labels: labels, Value: 42, TimeMS: at}}); err != nil {
		t.Fatal(err)
	}
	series, err := a.MetricSeries("gpuworks_gpu_utilization", at-1000)
	if err != nil || len(series) != 1 || len(series[0].Points) != 1 || series[0].Points[0].Value != 42 {
		t.Fatalf("series = %+v, %v", series, err)
	}
	if _, err := (&App{}).MetricSeries("x", 0); err == nil {
		t.Fatal("no metrics store, no error")
	}
}
