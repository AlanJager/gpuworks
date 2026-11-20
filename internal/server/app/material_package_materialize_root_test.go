//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These direct-consumer cases exercise real file descriptors during copying.
// The authenticated route derives a cancel context that does not forward Err;
// these cases therefore do not qualify that route's stage interleave.
func TestMaterialPackageMaterializeKeepsCapturedRootsDuringCopy(t *testing.T) {
	for _, fault := range []string{"data", "catalog", "stage"} {
		t.Run(fault, func(t *testing.T) {
			dataDir, receiver, manifest, _, digest := receivedPackageRootFixture(t)
			originalFiles := materialMaterializeSnapshotFiles(t, dataDir)
			replacement := t.TempDir()
			if err := os.WriteFile(filepath.Join(replacement, ".material-"+strings.Repeat("0", 32)), []byte("foreign stage candidate must survive"), 0600); err != nil {
				t.Fatal(err)
			}
			foreign := materialMaterializeSnapshotFiles(t, replacement)
			foreignRoot, err := os.Stat(replacement)
			if err != nil {
				t.Fatal(err)
			}
			observer := &materialMaterializeCopyContext{Context: t.Context(), t: t, dataDir: dataDir, replacement: replacement, fault: fault}
			err = materializeReceivedManifest(observer, receiver, manifest)
			if err == nil || observer.swaps != 1 {
				t.Fatalf("dynamic %s root replacement produced installation success or missed actual copy: err=%v swaps=%d", fault, err, observer.swaps)
			}
			currentForeign, err := os.Stat(observer.named)
			if err != nil || !os.SameFile(foreignRoot, currentForeign) || foreignRoot.Mode() != currentForeign.Mode() || !foreignRoot.ModTime().Equal(currentForeign.ModTime()) {
				t.Fatalf("foreign root identity/metadata changed: %v", err)
			}
			materialMaterializeAssertFiles(t, replacement, observer.named, foreign)
			originalRoot := dataDir
			if fault == "data" {
				originalRoot = observer.moved
			}
			materialMaterializeAssertFiles(t, dataDir, originalRoot, originalFiles)
			if fault != "stage" {
				catalogRoot := filepath.Join(originalRoot, "catalog-packages")
				if fault == "catalog" {
					catalogRoot = observer.moved
				}
				if _, err := os.Lstat(filepath.Join(catalogRoot, digest)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("data/catalog root refusal published a package", err)
				}
			}
			// Original owned staging directories can change. A stage namespace
			// replacement can be detected by deferred cleanup after an original
			// captured package was published; this test does not claim rollback.
			t.Logf("actual captured data/catalog/stage/output descriptors observed before %s rename; foreign root untouched and installation refused", fault)
		})
	}
}

type materialMaterializeCopyContext struct {
	context.Context
	t                           *testing.T
	dataDir, replacement, fault string
	named, moved                string
	swaps                       int
}

func (ctx *materialMaterializeCopyContext) Err() error {
	if ctx.swaps != 0 {
		return ctx.Context.Err()
	}
	frames := make([]uintptr, 32)
	n := runtime.Callers(1, frames)
	iterator := runtime.CallersFrames(frames[:n])
	reader, copy, install := false, false, false
	for {
		frame, more := iterator.Next()
		reader = reader || strings.HasSuffix(frame.Function, ".materialContextReader.Read")
		copy = copy || strings.HasSuffix(frame.Function, ".copyReceivedMaterialWithLink")
		install = install || (strings.HasSuffix(frame.Function, ".installReceivedPackage") || strings.HasSuffix(frame.Function, ".installReceivedPackageWithProgress"))
		if !more {
			break
		}
	}
	if !reader || !copy || !install {
		return ctx.Context.Err()
	}
	catalog := filepath.Join(ctx.dataDir, "catalog-packages")
	entries, err := os.ReadDir(catalog)
	if err != nil {
		ctx.t.Fatal(err)
	}
	stage := ""
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".material-package-") {
			if stage != "" {
				ctx.t.Fatal("multiple live received package stages")
			}
			stage = filepath.Join(catalog, entry.Name())
		}
	}
	if stage == "" {
		ctx.t.Fatal("actual copy has no created package stage")
	}
	output := ""
	if err := filepath.WalkDir(stage, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), ".material-") {
			if output != "" {
				return errors.New("multiple original copy output files")
			}
			output = name
		}
		return nil
	}); err != nil {
		ctx.t.Fatal(err)
	}
	if output == "" {
		ctx.t.Fatal("actual package copy has no original output file")
	}
	for _, name := range []string{ctx.dataDir, catalog, stage, output} {
		materialMaterializeAssertHeldFD(ctx.t, name)
	}
	ctx.named = ctx.dataDir
	if ctx.fault == "catalog" {
		ctx.named = catalog
	}
	if ctx.fault == "stage" {
		ctx.named = stage
	}
	ctx.moved = ctx.named + ".captured-original"
	if err := os.Rename(ctx.named, ctx.moved); err != nil {
		ctx.t.Fatal(err)
	}
	if err := os.Rename(ctx.replacement, ctx.named); err != nil {
		ctx.t.Fatal(err)
	}
	ctx.swaps++
	return ctx.Context.Err()
}

func materialMaterializeAssertHeldFD(t *testing.T, name string) {
	t.Helper()
	original, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		opened, err := os.Stat(filepath.Join("/proc/self/fd", entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(original, opened) {
			return
		}
	}
	t.Fatalf("actual material copy does not hold original FD %s", name)
}

type materialMaterializeFileSnapshot struct {
	info fs.FileInfo
	body []byte
}

func materialMaterializeSnapshotFiles(t *testing.T, root string) map[string]materialMaterializeFileSnapshot {
	t.Helper()
	files := map[string]materialMaterializeFileSnapshot{}
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		files[name] = materialMaterializeFileSnapshot{info, body}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func materialMaterializeAssertFiles(t *testing.T, beforeRoot, currentRoot string, before map[string]materialMaterializeFileSnapshot) {
	t.Helper()
	for name, old := range before {
		rel, err := filepath.Rel(beforeRoot, name)
		if err != nil {
			t.Fatal(err)
		}
		current := filepath.Join(currentRoot, rel)
		info, err := os.Stat(current)
		if err != nil || !os.SameFile(old.info, info) || old.info.Mode() != info.Mode() || !old.info.ModTime().Equal(info.ModTime()) {
			t.Fatalf("original or foreign file identity changed: %s %v", current, err)
		}
		body, err := os.ReadFile(current)
		if err != nil || string(body) != string(old.body) {
			t.Fatalf("original or foreign file bytes changed: %s %v", current, err)
		}
	}
	if beforeRoot != currentRoot {
		current := materialMaterializeSnapshotFiles(t, currentRoot)
		if len(current) != len(before) {
			t.Fatalf("foreign or relocated source file inventory changed: %d -> %d", len(before), len(current))
		}
	}
}
