//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReceivedMaterialEXDEVFallbackCancellationAndRetry(t *testing.T) {
	dataDir := t.TempDir()
	replicaDir := filepath.Join(dataDir, "material-replicas")
	if err := os.Mkdir(replicaDir, 0700); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(replicaDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("same verified bytes across a cross-device fallback retry")
	digest := materialTestDigest(body)
	if _, err := receiver.receive(t.Context(), digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dataDir, materialStageDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenRoot(replicaDir)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	name := "model-logos/exdev.png"
	consumer := filepath.Join(dataDir, name)
	centralStage := filepath.Join(dataDir, materialStageDirectory)
	localStage := filepath.Join(dataDir, path.Dir(name))
	assertNoStages := func() {
		t.Helper()
		for _, dir := range []string{centralStage, localStage} {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".material-") {
					t.Fatalf("temporary name remains in %s: %s", dir, entry.Name())
				}
			}
		}
	}
	checkStage := func(root *os.Root, stage string) {
		t.Helper()
		staged, err := root.Open(stage)
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := io.ReadAll(staged)
		if closeErr := staged.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("staged bytes differ: %q", got)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	firstLinks := 0
	linkAndCancel := func(_ context.Context, root *os.Root, stage, destination string) error {
		firstLinks++
		if path.Dir(stage) != materialStageDirectory || destination != name {
			t.Fatalf("unexpected first link %q -> %q", stage, destination)
		}
		checkStage(root, stage)
		cancel()
		return syscall.EXDEV
	}
	if err := copyReceivedMaterialWithLink(ctx, source, target, name, digest, int64(len(body)), true, linkAndCancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("EXDEV fallback ignored cancellation: %v", err)
	}
	if firstLinks != 1 {
		t.Fatalf("unexpected link attempts before cancellation: %d", firstLinks)
	}
	if _, err := os.Lstat(consumer); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled fallback published a consumer file: %v", err)
	}
	assertNoStages()

	linkAttempts := 0
	linkWithEXDEV := func(ctx context.Context, root *os.Root, stage, destination string) error {
		linkAttempts++
		checkStage(root, stage)
		if destination != name {
			t.Fatalf("unexpected destination %q", destination)
		}
		if linkAttempts == 1 {
			if path.Dir(stage) != materialStageDirectory {
				t.Fatalf("first retry stage was not central: %q", stage)
			}
			return syscall.EXDEV
		}
		if path.Dir(stage) != path.Dir(name) {
			t.Fatalf("fallback stage was not consumer-local: %q", stage)
		}
		return linkReceivedMaterial(ctx, root, stage, destination)
	}
	if err := copyReceivedMaterialWithLink(t.Context(), source, target, name, digest, int64(len(body)), true, linkWithEXDEV); err != nil {
		t.Fatalf("EXDEV retry failed: %v", err)
	}
	if linkAttempts != 2 {
		t.Fatalf("expected central EXDEV and real local link, got %d attempts", linkAttempts)
	}
	got, err := os.ReadFile(consumer)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("retry published wrong bytes: %q %v", got, err)
	}
	assertNoStages()
}
