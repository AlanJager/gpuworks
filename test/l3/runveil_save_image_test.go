//go:build docker

package l3

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// TestRunveilSavedImageLoadsIntoDocker proves a runveil node can be the source
// of a peer image sync to a Docker node: an image that came from `docker save`
// (the daemon's own layout), imported natively and saved
// by the udocker driver, loads into the real Docker daemon under a new name,
// preserves its config and rootfs, and runs. Runveil identifies the manifest;
// Docker's classic store identifies the config, while its containerd store
// identifies a manifest or index. Each digest must match its own content.
func TestRunveilSavedImageLoadsIntoDocker(t *testing.T) {
	source := l3AlpineImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	exported := filepath.Join(dir, "exported.tar")
	if output, err := exec.CommandContext(ctx, "docker", "save", "-o", exported, source).CombinedOutput(); err != nil {
		t.Fatalf("docker save: %v: %s", err, output)
	}
	sourceMembers := savedArchiveMembers(t, exported)
	var sourceManifest []struct{ Config string }
	if err := json.Unmarshal(sourceMembers["manifest.json"], &sourceManifest); err != nil || len(sourceManifest) != 1 {
		t.Fatalf("source archive must contain one image: %v", err)
	}
	sourceConfig := sourceMembers[sourceManifest[0].Config]
	if len(sourceConfig) == 0 {
		t.Fatal("source config missing")
	}
	configID := fmt.Sprintf("sha256:%x", sha256.Sum256(sourceConfig))
	var config struct {
		RootFS struct {
			Type    string   `json:"type"`
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(sourceConfig, &config); err != nil || config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) == 0 {
		t.Fatalf("source config has no valid rootfs: %v", err)
	}

	driver := udocker.New(filepath.Join(dir, "node"))
	if err := driver.LoadImageFile(ctx, source, exported); err != nil {
		t.Fatalf("runveil load of a docker save archive: %v", err)
	}
	inspection, err := driver.InspectImage(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("gpuworks/runveil-save-l3:%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", target).Run() })
	// Saved under a fresh tag so loading it cannot be mistaken for, or
	// overwrite, the image the archive came from.
	relabelled := filepath.Join(dir, "relabelled.tar")
	if err := relabel(ctx, driver, source, target, relabelled); err != nil {
		t.Fatal(err)
	}
	saved := savedArchiveMembers(t, relabelled)
	manifestBytes := saved["blobs/sha256/"+strings.TrimPrefix(inspection.ID, "sha256:")]
	if len(manifestBytes) == 0 || fmt.Sprintf("sha256:%x", sha256.Sum256(manifestBytes)) != inspection.ID {
		t.Fatal("saved manifest does not preserve runveil's content identity")
	}
	var manifest native.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Config.Digest != configID || len(manifest.Layers) != len(config.RootFS.DiffIDs) ||
		!bytes.Equal(saved["blobs/sha256/"+strings.TrimPrefix(configID, "sha256:")], sourceConfig) {
		t.Fatal("runveil save changed the source config or layer count")
	}
	for _, descriptor := range append([]native.Descriptor{manifest.Config}, manifest.Layers...) {
		blob := saved["blobs/sha256/"+strings.TrimPrefix(descriptor.Digest, "sha256:")]
		if int64(len(blob)) != descriptor.Size || fmt.Sprintf("sha256:%x", sha256.Sum256(blob)) != descriptor.Digest {
			t.Fatalf("saved blob differs from its manifest descriptor: %+v", descriptor)
		}
	}
	output, err := exec.CommandContext(ctx, "docker", "load", "-i", relabelled).CombinedOutput()
	if err != nil {
		t.Fatalf("docker load of a runveil-saved archive: %v: %s", err, output)
	}
	if !strings.Contains(string(output), target) {
		t.Fatalf("docker load did not report %s: %s", target, output)
	}
	for _, ref := range []string{source, target} {
		output, err := exec.CommandContext(ctx, "docker", "image", "inspect", ref).Output()
		if err != nil {
			t.Fatal(err)
		}
		var images []struct {
			ID         string
			Descriptor *native.Descriptor
			RootFS     struct {
				Type   string
				Layers []string
			}
		}
		if err := json.Unmarshal(output, &images); err != nil || len(images) != 1 {
			t.Fatalf("inspect %s: %v", ref, err)
		}
		members := saved
		if ref == source {
			members = sourceMembers
		}
		// Older Docker APIs omit Descriptor even for containerd image IDs.
		// Exercise that response too, using the same real exported content.
		for _, descriptor := range []*native.Descriptor{images[0].Descriptor, nil} {
			wantID := configID
			if descriptor == nil && images[0].ID != configID {
				blob, exists := members["blobs/sha256/"+strings.TrimPrefix(images[0].ID, "sha256:")]
				if !exists {
					t.Fatalf("Docker identity %s is neither the config nor an exported manifest/index for %s", images[0].ID, ref)
				}
				// No size was advertised by inspect. The image ID still must
				// hash these bytes and lead through verified descriptors to configID.
				descriptor = &native.Descriptor{Digest: images[0].ID, Size: int64(len(blob))}
			}
			if descriptor != nil {
				matches, err := archiveReferencesConfig(members, *descriptor, configID)
				if err != nil || !matches {
					t.Fatalf("Docker descriptor does not identify the exported config for %s: %+v, err=%v", ref, descriptor, err)
				}
				wantID = descriptor.Digest
			}
			if images[0].ID != wantID {
				t.Fatalf("Docker image ID %s differs from content identity %s for %s", images[0].ID, wantID, ref)
			}
		}
		if images[0].RootFS.Type != config.RootFS.Type || !slices.Equal(images[0].RootFS.Layers, config.RootFS.DiffIDs) {
			t.Fatalf("Docker config/rootfs differs from exported content for %s: %+v", ref, images[0])
		}
	}
	want, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network=none", "--pull=never", source, "cat", "/etc/alpine-release").CombinedOutput()
	if err != nil {
		t.Fatalf("run the source image: %v: %s", err, want)
	}
	release, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network=none", "--pull=never", target, "cat", "/etc/alpine-release").CombinedOutput()
	if err != nil || string(release) != string(want) {
		t.Fatalf("run the loaded image: %v: %s", err, release)
	}
}

// A containerd image ID can name an index, whose platform manifest names the
// config. Verify that chain using the exported bytes, including every traversed
// descriptor's size and hash. Archives may omit platforms not stored locally.
func archiveReferencesConfig(members map[string][]byte, descriptor native.Descriptor, configID string) (bool, error) {
	blob, exists := members["blobs/sha256/"+strings.TrimPrefix(descriptor.Digest, "sha256:")]
	if !exists {
		return false, nil
	}
	if int64(len(blob)) != descriptor.Size || fmt.Sprintf("sha256:%x", sha256.Sum256(blob)) != descriptor.Digest {
		return false, fmt.Errorf("descriptor content mismatch: %+v", descriptor)
	}
	var document struct {
		Config    native.Descriptor
		Manifests []native.Descriptor
	}
	if err := json.Unmarshal(blob, &document); err != nil {
		return false, err
	}
	if document.Config.Digest != "" {
		if document.Config.Digest != configID {
			return false, nil
		}
		config := members["blobs/sha256/"+strings.TrimPrefix(configID, "sha256:")]
		if int64(len(config)) != document.Config.Size || fmt.Sprintf("sha256:%x", sha256.Sum256(config)) != configID {
			return false, fmt.Errorf("config descriptor content mismatch: %+v", document.Config)
		}
		return true, nil
	}
	for _, child := range document.Manifests {
		matches, err := archiveReferencesConfig(members, child, configID)
		if err != nil || matches {
			return matches, err
		}
	}
	return false, nil
}

func savedArchiveMembers(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	members := map[string][]byte{}
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return members
		}
		if err != nil {
			t.Fatal(err)
		}
		if !header.FileInfo().Mode().IsRegular() {
			continue
		}
		if _, duplicate := members[header.Name]; duplicate {
			t.Fatalf("duplicate archive member %s", header.Name)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		members[header.Name] = data
	}
}

// relabel tags image as target on the runveil node and saves it to path.
func relabel(ctx context.Context, driver *udocker.Driver, image, target, path string) error {
	if err := driver.TagImage(ctx, image, target); err != nil {
		return fmt.Errorf("tag: %w", err)
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := driver.SaveImage(ctx, target, file); err != nil {
		file.Close()
		return fmt.Errorf("save %s: %w", target, err)
	}
	return file.Close()
}
