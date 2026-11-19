package pluginhost

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

func TestMetricsPropagatesOutputFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []MetricsRow
	}{
		{name: "empty report"},
		{name: "full report", rows: []MetricsRow{{ID: "acme_backup", State: string(StateRunning)}, {ID: "acme_off", State: "disabled"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHost(t, &fakeSpawner{}, nil, fastTiming())
			reader, writer := io.Pipe()
			t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
			failure := errors.New("metrics connection closed")
			if err := reader.CloseWithError(failure); err != nil {
				t.Fatal(err)
			}
			if err := h.WriteMetrics(writer, tc.rows); !errors.Is(err, failure) {
				t.Fatalf("output failure was lost: %v", err)
			}
		})
	}
}

// The exposition carries exactly the installed packages it is given, in
// id order, with zero counters for one never started; every state has a
// series, the histogram is cumulative, and an unknown table kind is not
// counted.
func TestMetricsExpositionGolden(t *testing.T) {
	h, _ := newTestHost(t, &fakeSpawner{}, nil, fastTiming())
	m := h.metricsFor("acme_backup")
	m.observeCall(Class{}, CallOK, 3*time.Millisecond)
	m.observeCall(Class{}, CallOK, 200*time.Millisecond)
	m.observeCall(Class{Admin: true}, CallBusy, 0)
	m.observeCall(Class{Stream: true}, CallTimeout, 61*time.Second)
	m.observeCall(Class{Admin: true, Stream: true}, CallFailed, time.Second)
	m.observeCall(Class{}, CallUnavailable, time.Millisecond)
	m.restarts.Add(2)
	h.CountPushRows("acme_backup", "banners", 3)
	h.CountPushRows("acme_backup", "node_holds", 2)
	h.CountPushRows("acme_backup", "banners", 1)
	h.CountPushRows("acme_backup", "no_such_kind", 5)
	h.metricsFor("acme_gone").observeCall(Class{}, CallOK, time.Millisecond) // uninstalled: not in rows

	var buf bytes.Buffer
	if err := h.WriteMetrics(&buf, []MetricsRow{{ID: "acme_off", State: "disabled"}, {ID: "acme_backup", State: string(StateRunning)}}); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "metrics.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("exposition differs from %s (go test -run TestMetricsExpositionGolden -update):\n%s", golden, buf.String())
	}
	if strings.Contains(buf.String(), "acme_gone") {
		t.Fatal("a package not in the rows was exposed")
	}
}

// Counting a call and a push allocates nothing.
func TestCountingAllocatesNothing(t *testing.T) {
	m := newPluginMetrics()
	classes := []Class{{}, {Admin: true}, {Stream: true}, {Admin: true, Stream: true}}
	n := 0
	allocs := testing.AllocsPerRun(1000, func() {
		n++
		m.observeCall(classes[n%len(classes)], CallOutcome(n%int(numOutcomes)), time.Duration(n)*time.Millisecond)
		m.addPushRows("node_holds", n%7)
	})
	if allocs != 0 {
		t.Fatalf("counting allocates %v times a call", allocs)
	}
}

// Do counts each call of a supervised plugin by class and outcome, and
// the counters survive a stop and a new start of the package;
// ForgetMetrics drops them.
func TestDoCountsCallsByOutcome(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_backup")
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/r/slow" {
				entered <- struct{}{}
				<-release
			}
			_, _ = io.WriteString(w, "ok")
		})
		return c
	}}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	spec := testSpec(t, "acme_backup")
	spec.MaxConcurrency = 1
	_ = h.Start(spec)
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := callPlugin(ctx, h, "acme_backup", "/r/x", Class{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	slow := make(chan error, 1)
	go func() {
		resp, err := callPlugin(ctx, h, "acme_backup", "/r/slow", Class{Admin: true})
		if err == nil {
			resp.Body.Close()
		}
		slow <- err
	}()
	<-entered
	if _, err := callPlugin(ctx, h, "acme_backup", "/r/x", Class{Admin: true}); productCode(t, err) != "PLUGIN_BUSY" {
		t.Fatalf("call while the one slot is held = %v", err)
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(ctx, "acme_backup"); err != nil {
		t.Fatal(err)
	}
	if _, err := callPlugin(ctx, h, "acme_backup", "/r/x", Class{}); productCode(t, err) != "PLUGIN_UNAVAILABLE" {
		t.Fatalf("call to a stopped plugin = %v", err)
	}
	if _, err := callPlugin(ctx, h, "acme_none", "/r/x", Class{}); err == nil {
		t.Fatal("a call to a plugin never supervised succeeded")
	}
	_ = h.Start(spec)
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	resp, err = callPlugin(ctx, h, "acme_backup", "/r/x", Class{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	m := h.metricsFor("acme_backup")
	for _, c := range []struct {
		class   int
		outcome CallOutcome
		want    uint64
	}{
		{classMember, CallOK, 2}, {classAdmin, CallOK, 1}, {classAdmin, CallBusy, 1}, {classMember, CallUnavailable, 1},
		{classMember, CallBusy, 0}, {classMemberStream, CallOK, 0},
	} {
		if got := m.calls[c.class][c.outcome].Load(); got != c.want {
			t.Errorf("%s/%s = %d, want %d", classNames[c.class], outcomeNames[c.outcome], got, c.want)
		}
	}
	var count uint64
	for b := range m.buckets[classMember] {
		count += m.buckets[classMember][b].Load()
	}
	if count != 3 || m.sumNanos[classMember].Load() == 0 {
		t.Fatalf("member calls timed %d times (sum %d ns), want 3", count, m.sumNanos[classMember].Load())
	}
	if _, ok := h.metrics["acme_none"]; ok {
		t.Fatal("a call to an unsupervised id made counters for it")
	}
	h.ForgetMetrics("acme_backup")
	h.CountPushRows("acme_backup", "banners", 2) // a push committed after the uninstall
	h.mu.Lock()
	_, kept := h.metrics["acme_backup"]
	h.mu.Unlock()
	if kept {
		t.Fatal("ForgetMetrics kept the counters, or a late push made them again")
	}
}

// metricsFor returns id's counters, made on first use.
func (h *Host) metricsFor(id string) *pluginMetrics {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.metricsLocked(id)
}

// A crash restart is counted; the first start and an on-demand start are
// not, even after the on-demand plugin crashed: nothing restarts it, its
// next start waits for a call.
func TestRestartsCountCrashRestartsOnly(t *testing.T) {
	sp := &fakeSpawner{config: okHandler("acme_backup"), spawnErr: func(n int) error {
		if n <= 2 {
			return errors.New("no such file")
		}
		return nil
	}}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	_ = h.Start(testSpec(t, "acme_backup"))
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	if got := h.metricsFor("acme_backup").restarts.Load(); got != 2 {
		t.Fatalf("restarts = %d after two failed starts, want 2", got)
	}

	od := &fakeSpawner{config: okHandler("acme_driver")}
	h2, _ := newTestHost(t, od, nil, fastTiming())
	_ = h2.Start(onDemandSpec(t, "acme_driver"))
	waitStatus(t, h2, "acme_driver", inState(StateIdle))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := callPlugin(ctx, h2, "acme_driver", "/r/x", Class{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := h2.metricsFor("acme_driver").restarts.Load(); got != 0 {
		t.Fatalf("an on-demand start counted %d restarts", got)
	}

	crashy := &fakeSpawner{config: okHandler("acme_driver"), spawnErr: func(n int) error {
		if n == 1 {
			return errors.New("no such file")
		}
		return nil
	}}
	h3, _ := newTestHost(t, crashy, nil, fastTiming())
	_ = h3.Start(onDemandSpec(t, "acme_driver"))
	waitStatus(t, h3, "acme_driver", inState(StateIdle))
	resp, err = callPlugin(ctx, h3, "acme_driver", "/r/x", Class{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if st, _ := h3.Status("acme_driver"); st.Starts != 2 {
		t.Fatalf("starts = %d, want a failed start and the one that served", st.Starts)
	}
	if got := h3.metricsFor("acme_driver").restarts.Load(); got != 0 {
		t.Fatalf("an on-demand plugin's crash counted %d restarts", got)
	}
}
