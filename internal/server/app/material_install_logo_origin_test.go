//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func receivedLogoTestRoots(t *testing.T, body []byte) (*os.Root, *os.Root, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	digest := materialTestDigest(body)
	sourceDir := filepath.Join(dir, "material-replicas")
	if err := os.MkdirAll(filepath.Join(sourceDir, digest[:2]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, digest[:2], digest), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, materialStageDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenRoot(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	target, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	name := "model-logos/" + digest + ".png"
	marker := "model-logos/" + receivedModelLogoOriginDirectory + "/" + digest + ".png"
	return source, target, digest, name, marker
}

func TestReceivedModelLogoFreshLinkMarksOnlyThatInode(t *testing.T) {
	body := []byte("fresh received content addressed model logo")
	source, target, digest, name, marker := receivedLogoTestRoots(t, body)
	if err := copyReceivedMaterial(t.Context(), source, target, name, digest, int64(len(body)), true); err != nil {
		t.Fatal(err)
	}
	finalInfo, err := target.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	markerInfo, err := target.Lstat(marker)
	if err != nil || !os.SameFile(finalInfo, markerInfo) {
		t.Fatalf("origin marker does not refer to the published inode: %v", err)
	}
	dir, err := target.Lstat("model-logos/" + receivedModelLogoOriginDirectory)
	if err != nil || !dir.IsDir() || dir.Mode().Perm()&0o077 != 0 {
		t.Fatalf("origin directory is not private: %v %v", dir, err)
	}
	if err := target.Remove(marker); err != nil {
		t.Fatal(err)
	}
	// The final name already exists. Rechecking the bytes cannot recreate the
	// marker or turn a legacy/crash-cut object into a local-origin claim.
	if err := copyReceivedMaterial(t.Context(), source, target, name, digest, int64(len(body)), true); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry invented local origin: %v", err)
	}
}

func TestReceivedModelLogoCrashCutAndLegacyNamesStayUnknown(t *testing.T) {
	body := []byte("source bytes for a crash cut")
	source, target, digest, name, marker := receivedLogoTestRoots(t, body)
	linkedThenFailed := func(ctx context.Context, root *os.Root, stage, final string) error {
		if err := linkReceivedMaterial(ctx, root, stage, final); err != nil {
			return err
		}
		return context.Canceled
	}
	if err := copyReceivedMaterialWithLink(t.Context(), source, target, name, digest, int64(len(body)), true, linkedThenFailed); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected simulated crash cut: %v", err)
	}
	if _, err := target.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash-cut publication gained marker: %v", err)
	}
	if err := copyReceivedMaterial(t.Context(), source, target, name, digest, int64(len(body)), true); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash-cut retry gained marker: %v", err)
	}
	for _, legacy := range []string{"model-logos/legacy.png", "model-logos/nested/" + digest + ".png", "model-logos/" + digest + ".jpeg", "model-logos/" + strings.Repeat("a", 64) + ".png"} {
		if err := copyReceivedMaterial(t.Context(), source, target, legacy, digest, int64(len(body)), true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := target.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unrecognized paths gained marker: %v", err)
	}
}

func TestReceivedModelLogoMarkerConflictLeavesFreshFinalUnmarked(t *testing.T) {
	body := []byte("valid received logo with foreign marker")
	source, target, digest, name, marker := receivedLogoTestRoots(t, body)
	if err := ensureMaterialParent(target, marker); err != nil {
		t.Fatal(err)
	}
	foreign, err := target.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	if err := copyReceivedMaterial(t.Context(), source, target, name, digest, int64(len(body)), true); !errors.Is(err, os.ErrExist) {
		t.Fatalf("foreign origin marker did not fail closed: %v", err)
	}
	finalInfo, err := target.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	markerInfo, err := target.Lstat(marker)
	if err != nil || os.SameFile(finalInfo, markerInfo) {
		t.Fatalf("failed publication falsely marked final inode: %v", err)
	}
	if err := copyReceivedMaterial(t.Context(), source, target, name, digest, int64(len(body)), true); err != nil {
		t.Fatalf("retry should verify existing final without origin claim: %v", err)
	}
}

func TestReceivedModelLogoEXDEVFallbackCreatesOriginMarker(t *testing.T) {
	body := []byte("cross device received logo")
	source, target, digest, name, marker := receivedLogoTestRoots(t, body)
	tries := 0
	link := func(ctx context.Context, root *os.Root, stage, final string) error {
		tries++
		if tries == 1 {
			return syscall.EXDEV
		}
		return linkReceivedMaterial(ctx, root, stage, final)
	}
	if err := copyReceivedMaterialWithLink(t.Context(), source, target, name, digest, int64(len(body)), true, link); err != nil {
		t.Fatal(err)
	}
	if tries != 2 {
		t.Fatalf("expected central stage and parent-local retry: %d", tries)
	}
	finalInfo, err := target.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	markerInfo, err := target.Lstat(marker)
	if err != nil || !os.SameFile(finalInfo, markerInfo) {
		t.Fatalf("EXDEV fallback lacked origin marker: %v", err)
	}
}
