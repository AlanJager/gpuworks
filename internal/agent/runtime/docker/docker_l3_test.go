//go:build docker

package docker

import (
	"context"
	"os"
	"testing"
)

// TestDockerInspectL3 exercises the real Docker daemon when explicitly
// requested. It is skipped by normal hermetic test runs and requires an image
// already available locally via GPUWORKS_TEST_IMAGE (for example
// alpine:3.20). This keeps CI deterministic while providing the planned L3
// verification gate for live/demo environments.
func TestDockerInspectL3(t *testing.T) {
	image := os.Getenv("GPUWORKS_TEST_IMAGE")
	if image == "" {
		t.Skip("set GPUWORKS_TEST_IMAGE and run go test -tags docker")
	}
	d := New()
	ok, err := d.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Skip("docker daemon is unavailable")
	}
	inspection, err := d.InspectImage(context.Background(), image)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.ID == "" {
		t.Fatalf("inspect %s returned empty image ID", image)
	}
	if tarPath := os.Getenv("GPUWORKS_TEST_IMAGE_TAR"); tarPath != "" {
		f, openErr := os.Open(tarPath)
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer f.Close()
		if err := d.LoadImage(context.Background(), image, f); err != nil {
			t.Fatal(err)
		}
		if after, inspectErr := d.InspectImage(context.Background(), image); inspectErr != nil || after.ID == "" {
			t.Fatalf("post-load inspect=%+v err=%v", after, inspectErr)
		}
	}
}
