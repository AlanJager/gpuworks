//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeReceivedManifestCleansKnownStagesWithoutOldManifestParent(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, "material-replicas"), 0700); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(filepath.Join(dataDir, "material-replicas"), 2)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("new logo")
	digest := materialTestDigest(body)
	if _, err := receiver.receive(t.Context(), digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	manifest := materialManifest{SourceMemberID: 1, Files: []materialManifestFile{{Class: MaterialModelLogo, Path: "model-logos/new.png", SHA256: digest, Bytes: int64(len(body))}}}
	stage := ".material-" + strings.Repeat("a", 32)
	write := func(name, contents string) string {
		t.Helper()
		full := filepath.Join(dataDir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		return full
	}
	orphans := []string{
		materialStageDirectory + "/" + stage,
		"model-logos/" + stage, // Legacy stage in the current manifest's parent.
	}
	for _, name := range orphans {
		write(name, "interrupted")
	}
	keep := []string{
		"images/old/partition/" + stage, // Legacy parent absent from this manifest.
		"benchmark-artifacts/" + stage,
		"dl/previous/" + stage,
		"images/old/partition/published.tar",
		"dl/previous/.material-keep",
		"benchmark-artifacts/.material-" + strings.Repeat("g", 32),
		materialStageDirectory + "/.material-keep",
	}
	for _, name := range keep {
		write(name, "keep")
	}
	packageStage := filepath.Join(dataDir, "catalog-packages", ".material-package-123456789")
	if err := os.MkdirAll(packageStage, 0700); err != nil {
		t.Fatal(err)
	}
	write("catalog-packages/.material-package-123456789/partial", "interrupted")
	write("catalog-packages/.material-package-123456789/nested/partial", "interrupted")
	outside := t.TempDir()
	outsideStage := filepath.Join(outside, stage)
	if err := os.WriteFile(outsideStage, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "images", "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(packageStage, "outside-link")); err != nil {
		t.Fatal(err)
	}
	changes := 0
	if err := materializeReceivedManifestWithChange(t.Context(), receiver, manifest, func() { changes++ }, nil); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if changes != 1 {
		t.Fatalf("new consumer path changed material notice count=%d", changes)
	}
	if err := materializeReceivedManifestWithChange(t.Context(), receiver, manifest, func() { changes++ }, nil); err != nil {
		t.Fatalf("idempotent materialize: %v", err)
	}
	if changes != 1 {
		t.Fatalf("idempotent materialize changed notice count=%d", changes)
	}
	for _, name := range orphans {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan %s remains: %v", name, err)
		}
	}
	if _, err := os.Lstat(packageStage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("package stage remains without current catalog package: %v", err)
	}
	for _, name := range keep {
		if got, err := os.ReadFile(filepath.Join(dataDir, name)); err != nil || string(got) != "keep" {
			t.Fatalf("unrelated %s changed: %q %v", name, got, err)
		}
	}
	if got, err := os.ReadFile(outsideStage); err != nil || string(got) != "outside" {
		t.Fatalf("symlink target changed: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dataDir, "model-logos", "new.png")); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("new material not installed: %q %v", got, err)
	}
	stageEntries, err := os.ReadDir(filepath.Join(dataDir, materialStageDirectory))
	if err != nil || len(stageEntries) != 1 || stageEntries[0].Name() != ".material-keep" {
		t.Fatalf("new installation left a generated stage: %v %v", stageEntries, err)
	}
}

func TestMaterialStageCleanupDoesNotWalkManagedTrees(t *testing.T) {
	dataDir := t.TempDir()
	current := filepath.Join(dataDir, "images")
	for i := 0; i < 66; i++ {
		current = filepath.Join(current, "nested")
	}
	if err := os.MkdirAll(current, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Mkdir(filepath.Join(dataDir, materialStageDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	if err := removeOrphanedMaterialStages(t.Context(), root, materialStageDirectory); err != nil {
		t.Fatalf("dedicated cleanup walked an unrelated deep tree: %v", err)
	}
}

func TestMaterialStageScanStopsOnCancellation(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, materialStageDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(dataDir, materialStageDirectory, ".material-"+strings.Repeat("c", 32))
	if err := os.WriteFile(stage, []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := removeOrphanedMaterialStages(ctx, root, materialStageDirectory); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup returned %v", err)
	}
	if got, err := os.ReadFile(stage); err != nil || string(got) != "incomplete" {
		t.Fatalf("cancelled cleanup changed stage: %q %v", got, err)
	}
}

func TestMaterializeReceivedManifestRejectsSymlinkedStageRoot(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, "material-replicas"), 0700); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(filepath.Join(dataDir, "material-replicas"), 2)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	stage := filepath.Join(outside, ".material-"+strings.Repeat("b", 32))
	if err := os.WriteFile(stage, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, materialStageDirectory)); err != nil {
		t.Fatal(err)
	}
	manifest := materialManifest{SourceMemberID: 1}
	if err := materializeReceivedManifest(t.Context(), receiver, manifest); err == nil {
		t.Fatal("symlinked stage root was accepted")
	}
	if got, err := os.ReadFile(stage); err != nil || string(got) != "outside" {
		t.Fatalf("symlink target changed: %q %v", got, err)
	}
}
