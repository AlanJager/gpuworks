package udocker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// cliFreeDriver is a provisioned runveil driver whose udocker CLI cannot be
// provisioned (no bundle, online install off): any CLI call fails with
// cliUnavailable, so a passing call proves the native path handled it.
func cliFreeDriver(t *testing.T) *Driver {
	t.Helper()
	t.Setenv(launcherEnv, "")
	d := New(t.TempDir())
	d.provisioned = true
	d.embeddedBundle = false
	d.bundleDir = ""
	waitNativeBackground(t, d)
	return d
}

// waitNativeBackground keeps the temp dir alive until background work of the
// driver's native engine (a base build where reflink works) has finished.
func waitNativeBackground(t *testing.T, d *Driver) {
	t.Cleanup(func() {
		if d.native != nil {
			d.native.WaitBackground()
		}
	})
}

const cliUnavailable = "udocker CLI unavailable"

// registerNativeImage writes a config and one layer blob and registers them
// under image, as a native pull would.
func registerNativeImage(t *testing.T, d *Driver, image, config string) {
	t.Helper()
	layout := native.Layout{Dir: d.udockerDir}
	if err := os.MkdirAll(layout.LayersDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(data []byte, mediaType string) native.Descriptor {
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
		if err := os.WriteFile(filepath.Join(layout.LayersDir(), digest), data, 0o644); err != nil {
			t.Fatal(err)
		}
		return native.Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(data))}
	}
	manifest := native.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config:        put([]byte(config), "application/vnd.oci.image.config.v1+json"),
		Layers:        []native.Descriptor{put([]byte("layer of "+image), "application/vnd.oci.image.layer.v1.tar")},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.RegisterImage(&native.Image{Ref: native.ParseRef(image), Manifest: manifest, ManifestJSON: data}); err != nil {
		t.Fatal(err)
	}
}

func TestInspectImageAnswersNativeImagesWithoutCLI(t *testing.T) {
	d := cliFreeDriver(t)
	registerNativeImage(t, d, "quay.io/team/app:v1", `{"config":{"Entrypoint":["/serve"],"Cmd":["--port","80"],"Env":["A=1","B=x=y"]}}`)
	inspection, err := d.InspectImage(context.Background(), "quay.io/team/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(inspection.ID, "sha256:") || !slices.Equal(inspection.Entrypoint, []string{"/serve"}) ||
		!slices.Equal(inspection.Cmd, []string{"--port", "80"}) || inspection.Env["A"] != "1" || inspection.Env["B"] != "x=y" {
		t.Fatalf("inspection %+v", inspection)
	}
	// An image only the CLI could know about still goes to the CLI.
	if _, err := d.InspectImage(context.Background(), "legacy/app:v1"); err == nil || !strings.Contains(err.Error(), cliUnavailable) {
		t.Fatalf("legacy image did not reach the CLI: %v", err)
	}
}

func TestTagAndRemoveNativeImagesWithoutCLI(t *testing.T) {
	d := cliFreeDriver(t)
	registerNativeImage(t, d, "mirror.example/team/app:v1", `{"config":{}}`)
	if err := d.TagImage(context.Background(), "mirror.example/team/app:v1", "team/app:v1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.ImageExists(context.Background(), "team/app:v1"); err != nil || !ok {
		t.Fatalf("canonical tag missing: %v %v", ok, err)
	}
	if err := d.RemoveImage(context.Background(), "mirror.example/team/app:v1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.ImageExists(context.Background(), "team/app:v1"); !ok {
		t.Fatal("removing the mirror name removed the canonical tag's blobs")
	}
	if err := d.RemoveImage(context.Background(), "team/app:v1"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(d.udockerDir, "layers"))
	if len(entries) != 0 {
		t.Fatalf("blobs left after removing every tag: %v", entries)
	}
	for _, call := range []func() error{
		func() error { return d.TagImage(context.Background(), "legacy/app:v1", "x/y:z") },
		func() error { return d.RemoveImage(context.Background(), "legacy/app:v1") },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), cliUnavailable) {
			t.Fatalf("legacy image did not reach the CLI: %v", err)
		}
	}
}

// The native engine fixes its file owner when it is first built, so no entry
// point may build it before provisioning has chosen the run user.
func TestTagImageProvisionsBeforeTouchingTheNativeEngine(t *testing.T) {
	t.Setenv(launcherEnv, "")
	d := New(t.TempDir())
	registerNativeImage(t, d, "mirror.example/team/app:v1", `{"config":{}}`)
	if err := d.TagImage(context.Background(), "mirror.example/team/app:v1", "team/app:v1"); err != nil {
		t.Fatal(err)
	}
	if !d.provisioned {
		t.Fatal("TagImage built the native engine before provisioning")
	}
}
