package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestNerdctlObservationUsesRecordedImageIdentity(t *testing.T) {
	target := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	for _, tc := range []struct {
		name, label, returnedTarget, imageID, imageError string
		containerStatus, imageStatus                     string
	}{
		{"recorded image", target, target, oldImageID, "", proto.ObservationObserved, proto.ObservationObserved},
		{"legacy name only", "", target, oldImageID, "", proto.ObservationObserved, proto.ObservationUnknown},
		{"image unavailable", target, target, "", "image metadata unavailable", proto.ObservationObserved, proto.ObservationUnknown},
		{"mismatched target", target, newImageID, oldImageID, "", proto.ObservationObserved, proto.ObservationUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$*" in
  '-n gpuworks container inspect --format {{json .}} lookup')
    printf '%%s\n' '{"Id":"%s","Image":"docker.io/example/runner:v1","Config":{"Labels":{"gpuworks.image-target":"%s"}}}' ;;
  '-n gpuworks image inspect --mode=native --format {{json .}} %s')
    if [ -n '%s' ]; then echo '%s' >&2; exit 1; fi
    printf '%%s\n' '{"Image":{"Target":{"digest":"%s"}},"ImageConfigDesc":{"digest":"%s"},"ImageConfig":{"config":{"Cmd":["serve"],"Env":["LD_LIBRARY_PATH=/usr/lib"],"Volumes":{}}}}' ;;
  *) echo 'unexpected lookup, including any lookup by mutable image name' >&2; exit 2 ;;
esac
`, oldContainerID, tc.label, target, tc.imageError, tc.imageError, tc.returnedTarget, tc.imageID)
			if err := os.WriteFile(filepath.Join(dir, "nerdctl"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			got := (&Engine{}).collectRuntimeObservation(context.Background(), nerdctl.New(), "lookup", oldContainerID)
			if got.Container.Status != tc.containerStatus || got.Image.Status != tc.imageStatus {
				t.Fatalf("observation=%+v", got)
			}
			if tc.imageStatus == proto.ObservationObserved {
				if got.Container.ImageID != oldImageID || got.Image.ImageID != oldImageID || got.Image.Env["LD_LIBRARY_PATH"] != "/usr/lib" {
					t.Fatalf("observed wrong image/config: %+v", got)
				}
			} else if got.Image.ImageID != "" || got.Container.ImageID != "" {
				t.Fatalf("unknown image must not contain a fabricated identity: %+v", got.Image)
			}
		})
	}
}

// Other drivers can report a config identity directly; resolving a lookup
// reference must not bypass their existing identity comparison.
type imageIdentityObservationDriver struct {
	headlessTestDriver
	workload runtime.WorkloadInspection
	imageID  string
	queried  string
}

func (d *imageIdentityObservationDriver) InspectWorkload(context.Context, string) (runtime.WorkloadInspection, error) {
	return d.workload, nil
}

func (d *imageIdentityObservationDriver) InspectImage(_ context.Context, ref string) (runtime.ImageInspection, error) {
	d.queried = ref
	return runtime.ImageInspection{ID: d.imageID}, nil
}

func TestObservationPreservesKnownImageIDValidation(t *testing.T) {
	for _, tc := range []struct {
		name, known, ref, actual, wantQuery, wantID, status string
	}{
		{"known ID", oldImageID, "", oldImageID, oldImageID, oldImageID, proto.ObservationObserved},
		{"known ID mismatch", oldImageID, "", newImageID, oldImageID, oldImageID, proto.ObservationUnknown},
		{"known ID mismatch through lookup", oldImageID, "immutable-ref", newImageID, "immutable-ref", oldImageID, proto.ObservationUnknown},
		{"missing identity and reference", "", "", oldImageID, "", "", proto.ObservationUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &imageIdentityObservationDriver{
				headlessTestDriver: headlessTestDriver{name: "test"},
				workload:           runtime.WorkloadInspection{ContainerID: oldContainerID, ImageID: tc.known, ImageLookupRef: tc.ref},
				imageID:            tc.actual,
			}
			got := (&Engine{}).collectRuntimeObservation(context.Background(), d, "lookup", oldContainerID)
			if got.Image.Status != tc.status || got.Container.ImageID != tc.wantID || d.queried != tc.wantQuery {
				t.Fatalf("observation=%+v query=%q", got, d.queried)
			}
		})
	}
}
