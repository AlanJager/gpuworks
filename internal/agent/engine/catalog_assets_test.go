package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func catalogAssetArchive(t *testing.T) []byte {
	t.Helper()
	var result bytes.Buffer
	gz := gzip.NewWriter(&result)
	tw := tar.NewWriter(gz)
	data := []byte("print('ready')\n")
	if err := tw.WriteHeader(&tar.Header{Name: "serve.py", Typeflag: tar.TypeReg, Mode: 0o777, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func TestApplyRuntimeAssetsPublishesReadOnlyCacheAndAcceptsContentlessHit(t *testing.T) {
	data := catalogAssetArchive(t)
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	stateDir := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(stateDir, func(name string, entry os.DirEntry, err error) error {
			if err == nil {
				if entry.IsDir() {
					_ = os.Chmod(name, 0o755)
				} else {
					_ = os.Chmod(name, 0o644)
				}
			}
			return nil
		})
	})
	engine := &Engine{stateDir: stateDir, log: slog.New(slog.DiscardHandler)}
	asset := proto.RuntimeAsset{File: "assets/runtime.tar.gz", SHA256: digest, SizeBytes: int64(len(data)), UnpackTo: "/opt/gpuworks/app", Content: data}
	workload := runtime.WorkloadSpec{}
	if err := engine.applyRuntimeAssets(&workload, desiredStateWithAssets(asset)); err != nil {
		t.Fatal(err)
	}
	if len(workload.Mounts) != 1 || workload.Mounts[0].Target != asset.UnpackTo || !workload.Mounts[0].ReadOnly {
		t.Fatalf("mounts=%+v", workload.Mounts)
	}
	info, err := os.Stat(filepath.Join(workload.Mounts[0].Source, "serve.py"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("cached mode=%o", info.Mode().Perm())
	}

	asset.Content = nil
	second := runtime.WorkloadSpec{}
	restarted := &Engine{stateDir: stateDir, log: slog.New(slog.DiscardHandler)}
	if err := restarted.applyRuntimeAssets(&second, desiredStateWithAssets(asset)); err != nil {
		t.Fatalf("agent restart cache hit required retransmission: %v", err)
	}
	if second.Mounts[0].Source != workload.Mounts[0].Source {
		t.Fatalf("cache source changed: %q != %q", second.Mounts[0].Source, workload.Mounts[0].Source)
	}
	if got := restarted.CachedRuntimeAssetDigests(); len(got) != 1 || got[0] != digest {
		t.Fatalf("advertised cache digests=%v, want %s", got, digest)
	}
}

func TestApplyRuntimeAssetsRejectsChecksumAndTargetCollisions(t *testing.T) {
	engine := &Engine{stateDir: t.TempDir(), log: slog.New(slog.DiscardHandler)}
	asset := proto.RuntimeAsset{File: "assets/serve.py", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("right"))), UnpackTo: "/opt/serve.py", Content: []byte("wrong")}
	if err := engine.applyRuntimeAssets(&runtime.WorkloadSpec{}, desiredStateWithAssets(asset)); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
	asset.Content = []byte("right")
	asset.SizeBytes = int64(len(asset.Content)) + 1
	if err := engine.applyRuntimeAssets(&runtime.WorkloadSpec{}, desiredStateWithAssets(asset)); err == nil {
		t.Fatal("content size mismatch was accepted")
	}
	asset.SizeBytes = int64(len(asset.Content))
	if err := engine.applyRuntimeAssets(&runtime.WorkloadSpec{}, desiredStateWithAssets(asset, asset)); err == nil {
		t.Fatal("duplicate mount target was accepted")
	}
}

func desiredStateWithAssets(assets ...proto.RuntimeAsset) proto.InstanceDesiredState {
	return proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{RuntimeAssets: assets}}
}
