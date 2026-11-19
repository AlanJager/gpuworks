package engine

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/agent/runtime/hostproc"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
	"gpuworks.ai/gpuworks/internal/proto"
)

// volumeDriver has named volumes and records the workload it was asked to
// run, then refuses it so the launch ends without a health wait.
type volumeDriver struct {
	imageToleranceDriver
	ran []runtime.WorkloadSpec
}

func (d *volumeDriver) NamedVolumes() bool { return true }

func (d *volumeDriver) Run(_ context.Context, spec runtime.WorkloadSpec) (string, error) {
	d.ran = append(d.ran, spec)
	return "", errors.New("recorded")
}

func volumeLaunch(uuid string, mounts ...proto.MountSpec) proto.InstanceDesiredState {
	spec := proto.InstanceDesiredState{UUID: uuid, Image: "repo:v1"}
	spec.Mounts = mounts
	return spec
}

// A named volume reaches the driver as the volume, not as an empty host path.
func TestNamedVolumeMountReachesTheDriver(t *testing.T) {
	driver := &volumeDriver{imageToleranceDriver: imageToleranceDriver{exists: []bool{true}}}
	e := NewWithProber(driver, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir(), injectionProber{})
	e.startOrAdopt(t.Context(), volumeLaunch("volume-run",
		proto.MountSpec{Volume: "myvol", Target: "/data"},
		proto.MountSpec{Source: "/srv/x", Target: "/x", ReadOnly: true}))
	if len(driver.ran) != 1 {
		t.Fatalf("runs = %d, want 1", len(driver.ran))
	}
	var args []string
	for _, mount := range driver.ran[0].Mounts {
		args = append(args, mount.VolumeArg())
	}
	if !slices.Contains(args, "myvol:/data") || !slices.Contains(args, "/srv/x:/x:ro") {
		t.Fatalf("mount args = %q", args)
	}
}

// A runtime without named volumes refuses the launch before pulling or
// creating anything, with the runtime and the volume as parameters.
func TestNamedVolumeMountRefusedWithoutRuntimeSupport(t *testing.T) {
	var statuses []proto.InstanceStatus
	driver := &imageToleranceDriver{exists: []bool{true}}
	driver.name = "udocker"
	e := NewWithProber(driver, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir(), injectionProber{})
	e.startOrAdopt(t.Context(), volumeLaunch("volume-refused", proto.MountSpec{Volume: "myvol", Target: "/data"}))
	f := failedReport(t, statuses).RuntimeFailure
	if f.Code != proto.ErrorMountVolumeUnsupported || f.Runtime != "udocker" || f.Resource != "myvol" || f.Phase != proto.PhaseContainerPrepare {
		t.Fatalf("reason = %+v", f)
	}
	if driver.runCalls != 0 || driver.pullCalls != 0 {
		t.Fatalf("refused launch reached the runtime: runs=%d pulls=%d", driver.runCalls, driver.pullCalls)
	}
}

// A volume that is not a volume name would be a host bind or extra mount
// options on the command line; it is refused even where volumes work.
func TestNamedVolumeMountRefusesNonVolumeNames(t *testing.T) {
	for name, mount := range map[string]proto.MountSpec{
		"host path":    {Volume: "/etc", Target: "/data"},
		"options":      {Volume: "vol:rw", Target: "/data"},
		"two sources":  {Volume: "myvol", Source: "/srv/x", Target: "/data"},
		"single char":  {Volume: "v", Target: "/data"},
		"leading dash": {Volume: "-v", Target: "/data"},
	} {
		t.Run(name, func(t *testing.T) {
			var statuses []proto.InstanceStatus
			driver := &volumeDriver{imageToleranceDriver: imageToleranceDriver{exists: []bool{true}}}
			e := NewWithProber(driver, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir(), injectionProber{})
			e.startOrAdopt(t.Context(), volumeLaunch("volume-invalid", mount))
			f := failedReport(t, statuses).RuntimeFailure
			if f.Code != proto.ErrorMountVolumeInvalid || f.Resource != mount.Volume {
				t.Fatalf("reason = %+v", f)
			}
			if len(driver.ran) != 0 {
				t.Fatal("refused launch reached the runtime")
			}
		})
	}
}

// Which real drivers run named volumes: the ones whose CLI owns them.
func TestRealDriversClaimNamedVolumesOnlyWhereTheRuntimeHasThem(t *testing.T) {
	for _, tc := range []struct {
		driver runtime.Driver
		want   bool
	}{
		{docker.New(), true},
		{nerdctl.New(), true},
		{udocker.New(t.TempDir()), false},
		{hostproc.New(t.TempDir()), false},
		{kube.New(kube.Options{}), false},
	} {
		if got := runtime.SupportsNamedVolumes(tc.driver); got != tc.want {
			t.Errorf("%s: named volumes = %v, want %v", tc.driver.Name(), got, tc.want)
		}
		err := checkNamedVolumes(volumeLaunch("x", proto.MountSpec{Volume: "myvol", Target: "/data"}), tc.driver)
		if (err == nil) != tc.want {
			t.Errorf("%s: check = %v", tc.driver.Name(), err)
		}
	}
}
