package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type imageArchiveTestDriver struct {
	imagePullTestDriver
	present  bool
	loadErr  error
	saveErr  error
	loaded   string
	saveData string
}

func (d *imageArchiveTestDriver) ImageExists(context.Context, string) (bool, error) {
	return d.present, nil
}

func (d *imageArchiveTestDriver) LoadImage(_ context.Context, _ string, r io.Reader) error {
	data, err := io.ReadAll(r)
	d.loaded = string(data)
	if err != nil {
		return err
	}
	return d.loadErr
}

func (d *imageArchiveTestDriver) SaveImage(_ context.Context, _ string, w io.Writer) error {
	if d.saveErr != nil {
		return d.saveErr
	}
	_, err := io.WriteString(w, d.saveData)
	return err
}

func TestStreamImageSaveLoadAndFailures(t *testing.T) {
	unsupported := NewNodeRuntime(slog.Default(), &imagePullTestDriver{}, t.TempDir())
	if err := unsupported.StreamImage(context.Background(), "image:test", "xfer", &bytes.Buffer{}, false); err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("unsupported image archive error = %v", err)
	}

	driver := &imageArchiveTestDriver{present: true, saveData: "archive"}
	n := NewNodeRuntime(slog.Default(), driver, t.TempDir())
	var statuses []proto.ImageNodeStatus
	n.Bind(func(kind string, body any) error {
		if kind == proto.TStatusImageNode {
			statuses = append(statuses, body.(proto.ImageNodeStatus))
		}
		return nil
	})
	var saved bytes.Buffer
	if err := n.StreamImage(context.Background(), " image:test ", "save-1", &saved, false); err != nil || saved.String() != "archive" {
		t.Fatalf("save image data=%q err=%v", saved.String(), err)
	}
	load := bytes.NewBufferString("loaded-archive")
	if err := n.StreamImage(context.Background(), " image:test ", "load-1", load, true); err != nil {
		t.Fatal(err)
	}
	if driver.loaded != "loaded-archive" || len(statuses) != 2 || statuses[0].State != "syncing" || statuses[1].State != "present" {
		t.Fatalf("loaded=%q statuses=%+v", driver.loaded, statuses)
	}
	if err := n.StreamImage(context.Background(), "", "empty", &bytes.Buffer{}, false); err == nil {
		t.Fatal("empty image ref unexpectedly accepted")
	}

	driver.loadErr = errors.New("broken archive")
	if err := n.StreamImage(context.Background(), "image:test", "load-error", bytes.NewBufferString("bad"), true); !errors.Is(err, driver.loadErr) {
		t.Fatalf("load error = %v", err)
	}
	driver.loadErr = nil
	driver.present = false
	if err := n.StreamImage(context.Background(), "image:test", "missing", bytes.NewBufferString("ok"), true); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("missing loaded image error = %v", err)
	}
	driver.saveErr = errors.New("save failed")
	if err := n.StreamImage(context.Background(), "image:test", "save-error", &bytes.Buffer{}, false); !errors.Is(err, driver.saveErr) {
		t.Fatalf("save error = %v", err)
	}
}

func TestDoctorAndSmallMetricHelpers(t *testing.T) {
	var report strings.Builder
	Doctor(context.Background(), &report, &imagePullTestDriver{})
	for _, want := range []string{"gpuworks agent doctor", "-- runtimes --", "image-pull-test (selected)", "-- accelerators --"} {
		if !strings.Contains(report.String(), want) {
			t.Fatalf("doctor report lacks %q:\n%s", want, report.String())
		}
	}
	if got := checkCmd(context.Background(), "gpuworks-command-that-does-not-exist"); got != "not found" {
		t.Fatalf("missing command = %q", got)
	}
	if got := trim("value \r\n"); got != "value" {
		t.Fatalf("trim = %q", got)
	}
	if got := gib(1024 * 1024); got != 1 {
		t.Fatalf("gib = %v", got)
	}

}

func TestDoctorNVIDIADiagnosticsAreManualAndExplicit(t *testing.T) {
	p := &gpu.FixedProber{VendorName: "nvidia", GPUs: []proto.GPUInfo{{Index: 0, Vendor: "nvidia", Name: "A100", Healthy: true}}}
	var report strings.Builder
	doctorWithProber(context.Background(), &report, p)
	got := report.String()
	for _, want := range []string{
		"-- facts --",
		"fabric manager",
		"cuda runtime",
		"peer access       SKIP (fewer than 2 GPUs)",
		"nccl              SKIP (fewer than 2 GPUs)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("doctor report lacks %q:\n%s", want, got)
		}
	}
}

type doctorDeviceDriver struct {
	imagePullTestDriver
	spec runtime.WorkloadSpec
}

func (d *doctorDeviceDriver) DeviceAccessDiagnostics(spec runtime.WorkloadSpec) string {
	d.spec = spec
	return "device access: PASS (fixture)"
}

func TestDoctorChecksSelectedRuntimeDeviceAccess(t *testing.T) {
	p := &gpu.FixedProber{VendorName: "ascend", GPUs: []proto.GPUInfo{{Index: 3, Vendor: "ascend", Name: "910B", Healthy: true}}}
	driver := &doctorDeviceDriver{}
	var report strings.Builder
	doctorWithProber(context.Background(), &report, p, driver)
	if !strings.Contains(report.String(), "device access: PASS (fixture)") {
		t.Fatalf("doctor report lacks device access result:\n%s", report.String())
	}
	if !slices.Equal(driver.spec.GPUIndexes, []int{3}) || driver.spec.Env["ASCEND_VISIBLE_DEVICES"] != "3" {
		t.Fatalf("doctor workload spec = %+v", driver.spec)
	}
}

func TestDoctorReportsSelectedRuntimePlugin(t *testing.T) {
	// Vendor detection itself lives on the real probers (see the gpu package
	// tests); this exercises the doctor wiring: a detected-but-not-installed
	// plugin must surface as a runtime-plugin status line.
	p := doctorPluginProber{
		FixedProber: &gpu.FixedProber{VendorName: "ascend", GPUs: []proto.GPUInfo{{Index: 0, Vendor: "ascend", Name: "910B", Healthy: true}}},
		plugin:      runtime.PluginInfo{Name: "Ascend Docker Runtime"},
	}
	d := &doctorDockerDriver{}
	var report strings.Builder
	doctorWithProber(context.Background(), &report, p, d)
	if !strings.Contains(report.String(), "runtime plugin    Ascend Docker Runtime (not installed)") {
		t.Fatalf("doctor report lacks runtime plugin status:\n%s", report.String())
	}
}

type doctorPluginProber struct {
	*gpu.FixedProber
	plugin runtime.PluginInfo
}

func (p doctorPluginProber) DetectRuntimePlugin(context.Context, string) runtime.PluginInfo {
	return p.plugin
}

type doctorDockerDriver struct{ imagePullTestDriver }

func (*doctorDockerDriver) Name() string { return "docker" }

func TestForcedRuntimeConstructorsAndModelSyncStopNotRunning(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, force := range []string{"docker", "nerdctl", "udocker", "host"} {
		got := detectDriver(log, t.TempDir(), force, nil).driver
		if driverUnavailableOn(goruntime.GOOS, force) != "" {
			// host is macOS only and udocker Linux only (TestForcedDriverMustSuitTheOS).
			if got != nil {
				t.Errorf("forced %s on %s = %T, want none", force, goruntime.GOOS, got)
			}
			continue
		}
		if got == nil || got.Name() != force {
			t.Errorf("forced %s driver = %T/%v", force, got, got)
		}
	}
	n := newNodeRuntime(log, nil, t.TempDir(), "none", metricsOnlyProber{}, nil)
	result, err := n.rpcModelSyncStop([]byte(`{"key":"missing","transfer_id":"xfer"}`))
	if err != nil || result.(map[string]string)["status"] != "not_running" {
		t.Fatalf("model sync stop = %v err=%v", result, err)
	}
}

var _ runtime.ImageArchiver = (*imageArchiveTestDriver)(nil)
