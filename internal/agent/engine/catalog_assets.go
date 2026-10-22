package engine

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/catalogasset"
)

func (e *Engine) applyRuntimeAssets(workload *runtime.WorkloadSpec, spec proto.InstanceDesiredState) error {
	if len(spec.RuntimeAssets) == 0 {
		return nil
	}
	e.log.Info("preparing runtime assets", "uuid", spec.UUID, "count", len(spec.RuntimeAssets))
	e.assetMu.Lock()
	defer e.assetMu.Unlock()

	seenTargets := map[string]struct{}{}
	for index, asset := range spec.RuntimeAssets {
		asset.File = path.Clean(strings.ReplaceAll(strings.TrimSpace(asset.File), "\\", "/"))
		asset.SHA256 = strings.ToLower(strings.TrimSpace(asset.SHA256))
		asset.UnpackTo = strings.TrimSpace(asset.UnpackTo)
		if !strings.HasPrefix(asset.File, "assets/") || !catalogasset.AllowedFile(strings.TrimSuffix(asset.File, ".tar.gz")) && !strings.HasSuffix(strings.ToLower(asset.File), ".tar.gz") {
			return fmt.Errorf("runtime_assets[%d] has forbidden file %q", index, asset.File)
		}
		if len(asset.SHA256) != 64 || strings.Trim(asset.SHA256, "0123456789abcdef") != "" {
			return fmt.Errorf("runtime_assets[%d] has invalid SHA-256", index)
		}
		if !strings.HasPrefix(asset.UnpackTo, "/") || path.Clean(asset.UnpackTo) != asset.UnpackTo || asset.UnpackTo == "/" {
			return fmt.Errorf("runtime_assets[%d] has invalid unpack_to %q", index, asset.UnpackTo)
		}
		if _, duplicate := seenTargets[asset.UnpackTo]; duplicate {
			return fmt.Errorf("runtime_assets[%d] duplicates mount target %q", index, asset.UnpackTo)
		}
		seenTargets[asset.UnpackTo] = struct{}{}

		isArchive := strings.HasSuffix(strings.ToLower(asset.File), ".tar.gz")
		var source string
		var err error
		if isArchive {
			source, err = ensureArchiveAsset(e.runtimeMountManager(), asset)
		} else {
			source, err = ensureFileAsset(e.runtimeMountManager(), asset)
		}
		if err != nil {
			return fmt.Errorf("runtime asset %s: %w", asset.File, err)
		}
		workload.Mounts = append(workload.Mounts, runtime.Mount{Source: source, Target: asset.UnpackTo, ReadOnly: true})
	}
	return nil
}

func ensureArchiveAsset(manager *RuntimeMountManager, asset proto.RuntimeAsset) (string, error) {
	owner, destination, err := manager.assetPath(asset.SHA256, ".dir")
	if err != nil {
		return "", err
	}
	cacheRoot := filepath.Dir(destination)
	marker := filepath.Join(cacheRoot, asset.SHA256+".complete")
	if validArchiveCache(destination, marker, asset.SHA256) {
		if err := manager.finalizeReadOnly(destination, owner); err != nil {
			return "", err
		}
		return destination, nil
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", errors.New("existing archive cache is incomplete or invalid")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := verifyRuntimeAssetContent(asset); err != nil {
		return "", err
	}
	if err := catalogasset.ExtractTarGzip(bytes.NewReader(asset.Content), destination); err != nil {
		return "", err
	}
	if err := manager.finalizeReadOnly(destination, owner); err != nil {
		return "", fmt.Errorf("make archive cache read-only: %w", err)
	}
	if err := os.WriteFile(marker, []byte(asset.SHA256+"\n"), 0o444); err != nil {
		return "", fmt.Errorf("write archive cache marker: %w", err)
	}
	return destination, nil
}

func validArchiveCache(destination, marker, digest string) bool {
	info, err := os.Lstat(destination)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	markerInfo, err := os.Lstat(marker)
	if err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 {
		return false
	}
	data, err := os.ReadFile(marker)
	return err == nil && strings.TrimSpace(string(data)) == digest
}

func ensureFileAsset(manager *RuntimeMountManager, asset proto.RuntimeAsset) (string, error) {
	owner, destination, err := manager.assetPath(asset.SHA256, ".file")
	if err != nil {
		return "", err
	}
	cacheRoot := filepath.Dir(destination)
	if matchesFileDigest(destination, asset.SHA256) {
		if err := manager.finalizeReadOnly(destination, owner); err != nil {
			return "", err
		}
		return destination, nil
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", errors.New("existing file cache has the wrong digest or type")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := verifyRuntimeAssetContent(asset); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(cacheRoot, ".asset-*")
	if err != nil {
		return "", err
	}
	temporaryName := temporary.Name()
	published := false
	defer func() {
		_ = temporary.Close()
		if !published {
			_ = os.Remove(temporaryName)
		}
	}()
	if _, err := temporary.Write(asset.Content); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(temporaryName, owner.UID, owner.GID); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(temporaryName, 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return "", err
	}
	published = true
	if err := manager.preflight(destination, owner, false); err != nil {
		return "", err
	}
	return destination, nil
}

func verifyRuntimeAssetContent(asset proto.RuntimeAsset) error {
	if len(asset.Content) == 0 {
		return errors.New("cache miss and desired state contains no asset content")
	}
	if int64(len(asset.Content)) > catalogasset.MaxFileBytes {
		return fmt.Errorf("content exceeds %d bytes", catalogasset.MaxFileBytes)
	}
	if asset.SizeBytes > 0 && int64(len(asset.Content)) != asset.SizeBytes {
		return fmt.Errorf("content size mismatch: got %d, want %d", len(asset.Content), asset.SizeBytes)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(asset.Content))
	if digest != asset.SHA256 {
		return fmt.Errorf("content SHA-256 mismatch: got %s", digest)
	}
	return nil
}

func matchesFileDigest(name, digest string) bool {
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > catalogasset.MaxFileBytes {
		return false
	}
	file, err := os.Open(name)
	if err != nil {
		return false
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, catalogasset.MaxFileBytes+1)); err != nil {
		return false
	}
	return fmt.Sprintf("%x", hash.Sum(nil)) == digest
}

// CachedRuntimeAssetDigests returns only cache entries that the normal launch
// path would accept without content. It is called once per handshake, so a
// reconnect can resume without retransmitting immutable assets.
func (e *Engine) CachedRuntimeAssetDigests() []string {
	if e == nil {
		return nil
	}
	e.assetMu.Lock()
	defer e.assetMu.Unlock()
	cacheRoot := e.runtimeMountManager().purposeRoot("assets")
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, ".file"):
			digest := strings.TrimSuffix(name, ".file")
			if len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == "" && matchesFileDigest(filepath.Join(cacheRoot, name), digest) {
				seen[digest] = struct{}{}
			}
		case strings.HasSuffix(name, ".dir"):
			digest := strings.TrimSuffix(name, ".dir")
			if len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == "" && validArchiveCache(filepath.Join(cacheRoot, name), filepath.Join(cacheRoot, digest+".complete"), digest) {
				seen[digest] = struct{}{}
			}
		}
	}
	digests := make([]string, 0, len(seen))
	for digest := range seen {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	return digests
}
