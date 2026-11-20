//go:build linux && dqlite

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/store"
)

const materialStageDirectory = ".material-stages"

// checkMaterialConsumerReferences refuses to acknowledge bytes whose current
// database consumer would still read an old member's absolute path. Benchmark
// references need a portable DB contract before cross-host installation can
// succeed; merely copying the file does not make the API or tokenizer use it.
// Current catalog package references must also occur in the verified snapshot.
func checkMaterialConsumerReferences(ctx context.Context, st *store.Store, receiver *materialReceiver, manifest materialManifest) error {
	if st == nil || receiver == nil {
		return errors.New("material consumer references require a member store")
	}
	root := filepath.Join(filepath.Dir(receiver.root), "benchmark-artifacts")
	paths := make(map[string]MaterialClass)
	offlineFiles := make(map[string]materialManifestFile)
	catalogPackages := make(map[string]bool)
	for _, file := range manifest.Files {
		if file.Class == MaterialBenchmarkInput || file.Class == MaterialBenchmarkDetail {
			paths[file.Path] = file.Class
		} else if file.Class == MaterialOfflineImage {
			offlineFiles[file.Path] = file
		} else if file.Class == MaterialCatalogPackage {
			parts := strings.SplitN(file.Path, "/", 3)
			if len(parts) != 3 || !validMaterialDigest(parts[1]) || !validMaterialConsumerPath(file.Class, file.Path) {
				return errors.New("catalog material path has no valid package digest")
			}
			catalogPackages[parts[1]] = true
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	digests, err := st.CompleteCatalogPackageReferences(ctx)
	if err != nil {
		return err
	}
	for _, digest := range digests {
		if !catalogPackages[digest] {
			return errors.New("current catalog package reference is absent from the material manifest")
		}
	}
	images, err := st.OfflineImageBlobRows(ctx)
	if err != nil {
		return err
	}
	referencedImages := make(map[string]bool, len(images))
	for _, image := range images {
		name := image.OfflineBlobPath
		if filepath.Base(name) != name || name == "." || name == ".." || !validMaterialDigest(image.OfflineBlobSHA256) || image.OfflineBlobBytes <= 0 {
			return errors.New("offline image catalog reference is invalid")
		}
		path := "images/" + name
		file, ok := offlineFiles[path]
		if !ok || file.SHA256 != image.OfflineBlobSHA256 || file.Bytes != image.OfflineBlobBytes {
			return errors.New("offline image catalog reference differs from the current material manifest")
		}
		referencedImages[path] = true
	}
	if len(referencedImages) != len(offlineFiles) {
		return errors.New("offline image material manifest contains unreferenced bytes")
	}
	check := func(ref string, class MaterialClass) error {
		if ref == "" || filepath.Clean(ref) != ref {
			return errors.New("benchmark material reference is not canonical")
		}
		if filepath.IsAbs(ref) {
			if filepath.Dir(ref) != root {
				return errors.New("benchmark material reference still names another member's path")
			}
			ref = filepath.Base(ref)
		}
		if ref == "." || ref == ".." || filepath.Base(ref) != ref || strings.ContainsAny(ref, "/\\\x00") {
			return errors.New("benchmark material reference is not a plain file name")
		}
		if paths["benchmark-artifacts/"+ref] != class {
			return errors.New("benchmark material reference is absent from the current manifest")
		}
		return nil
	}
	runs, err := st.BenchmarkTokenizerRows(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if strings.HasPrefix(run.TokenizerRef, "hf://") || strings.HasPrefix(run.TokenizerRef, "ms://") {
			continue
		}
		if parsed, err := url.Parse(run.TokenizerRef); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			continue
		}
		ref := run.TokenizerRef
		if strings.HasPrefix(ref, "artifact://") {
			ref = strings.TrimPrefix(ref, "artifact://")
			if !strings.HasPrefix(ref, "tokenizer-") || !strings.HasSuffix(ref, ".json") {
				return errors.New("invalid uploaded tokenizer reference")
			}
		}
		if err := check(ref, MaterialBenchmarkInput); err != nil {
			return err
		}
	}
	points, err := st.BenchmarkDetailRows(ctx)
	if err != nil {
		return err
	}
	for _, point := range points {
		if err := check(point.DetailPath, MaterialBenchmarkDetail); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// materializeReceivedManifest publishes only shared business bytes. The
// caller must have just verified the manifest against live membership and
// local replicas. A partial installation never changes member readiness;
// repeating this operation accepts identical files and rejects conflicts.
func materializeReceivedManifest(ctx context.Context, receiver *materialReceiver, manifest materialManifest) error {
	return materializeReceivedManifestWithChange(ctx, receiver, manifest, nil, nil)
}

// dataRoot is the consumer directory the caller already verified. When it is
// set, installation keeps that directory and does not open the path again.
// A nil root opens the consumer path for a caller that holds no directory.
func materializeReceivedManifestWithChange(ctx context.Context, receiver *materialReceiver, manifest materialManifest, onChange func(), dataRoot *os.Root) error {
	return materializeReceivedManifestWithProgress(ctx, receiver, manifest, onChange, dataRoot, nil)
}

// Progress follows completed, verified work; it never acknowledges a manifest.
func materializeReceivedManifestWithProgress(ctx context.Context, receiver *materialReceiver, manifest materialManifest, onChange func(), dataRoot *os.Root, progress func() error) error {
	if receiver == nil || manifest.SourceMemberID == 0 || manifest.SourceMemberID == receiver.memberID {
		return errors.New("material installation requires a verified remote source")
	}
	dataDir := filepath.Dir(receiver.root)
	if filepath.Base(receiver.root) != "material-replicas" {
		return errors.New("material installation requires the managed replica root")
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var target *os.Root
	if dataRoot != nil {
		// The verified directory can already have been replaced by path.
		// Opening either root before this check would bind the replacement.
		if err := checkMaterialDataRoot(dataDir, dataRoot); err != nil {
			return err
		}
		target = dataRoot
	}
	source, err := os.OpenRoot(receiver.root)
	if err != nil {
		return err
	}
	defer source.Close()
	if target == nil {
		target, err = os.OpenRoot(dataDir)
		if err != nil {
			return err
		}
		defer target.Close()
	}

	packagesByDigest := make(map[string][]materialManifestFile)
	catalogPaths := make(map[string]materialManifestFile)
	var individual []materialManifestFile
	for _, file := range manifest.Files {
		if !validMaterialConsumerPath(file.Class, file.Path) || !validMaterialDigest(file.SHA256) || file.Bytes <= 0 || file.Bytes > maxMaterialReplicaBytes {
			return errors.New("material installation received an invalid manifest entry")
		}
		switch file.Class {
		case MaterialMasterSecret, MaterialMemberIdentity:
			// The target's master was checked against the manifest already.
			// Member keys, certificate and listener config belong to this target.
		case MaterialCatalogPackage:
			parts := strings.SplitN(file.Path, "/", 3)
			if len(parts) != 3 || !validMaterialDigest(parts[1]) || parts[2] == "" {
				return errors.New("catalog material path has no package digest")
			}
			packagesByDigest[parts[1]] = append(packagesByDigest[parts[1]], file)
			catalogPaths[file.Path] = file
		case MaterialRuntimeAsset:
			// Runtime assets are already included in the catalog package tree.
		case MaterialOfflineImage, MaterialBenchmarkInput, MaterialBenchmarkDetail,
			MaterialModelLogo, MaterialSSHHostKeys, MaterialAgentOverride:
			individual = append(individual, file)
		default:
			return fmt.Errorf("unknown material class %q", file.Class)
		}
	}
	for _, file := range manifest.Files {
		if file.Class != MaterialRuntimeAsset {
			continue
		}
		catalog, ok := catalogPaths[file.Path]
		if !ok || catalog.SHA256 != file.SHA256 || catalog.Bytes != file.Bytes {
			return errors.New("runtime asset is absent from its catalog package")
		}
	}
	if err := ensureMaterialParent(target, materialStageDirectory+"/stage"); err != nil {
		return err
	}
	if err := removeOrphanedMaterialStages(ctx, target, materialStageDirectory); err != nil {
		return err
	}
	if _, err := target.Lstat("catalog-packages"); err == nil {
		if err := removeOrphanedMaterialPackageStages(ctx, target); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Install complete packages before references such as logos and benchmark
	// details. The package store validates the full staged tree and publishes a
	// digest directory atomically, including assets and documentation.
	digests := make([]string, 0, len(packagesByDigest))
	for digest := range packagesByDigest {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	for _, digest := range digests {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := "catalog-packages/" + digest
		_, beforeErr := target.Lstat(name)
		installErr := installReceivedPackageWithProgress(ctx, source, target, dataDir, digest, packagesByDigest[digest], progress)
		notifyReceivedMaterialPublication(target, name, beforeErr, onChange)
		if installErr != nil {
			return fmt.Errorf("install catalog package %s: %w", digest, installErr)
		}
		if err := completedMaterialInstallationStage(ctx, progress); err != nil {
			return err
		}
	}
	cleanedParents := make(map[string]bool)
	for _, file := range individual {
		parent := path.Dir(file.Path)
		if !cleanedParents[parent] {
			if err := ensureMaterialParent(target, file.Path); err != nil {
				return err
			}
			if err := removeOrphanedMaterialStages(ctx, target, parent); err != nil {
				return err
			}
			cleanedParents[parent] = true
		}
		_, beforeErr := target.Lstat(file.Path)
		installErr := copyReceivedMaterial(ctx, source, target, file.Path, file.SHA256, file.Bytes, true)
		notifyReceivedMaterialPublication(target, file.Path, beforeErr, onChange)
		if installErr != nil {
			return fmt.Errorf("install %s material %s: %w", file.Class, file.Path, installErr)
		}
		if progress != nil {
			if err := checkMaterialDataRoot(dataDir, target); err != nil {
				return err
			}
			if err := completedMaterialInstallationStage(ctx, progress); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func notifyReceivedMaterialPublication(target *os.Root, name string, beforeErr error, onChange func()) {
	if onChange == nil || !errors.Is(beforeErr, os.ErrNotExist) {
		return
	}
	if _, err := target.Lstat(name); err == nil {
		onChange()
	}
}

// The receiver serializes installation under its lock. New individual files
// stage in one known directory, so retry cleanup need not walk consumer trees.
// Legacy stages in an old manifest's absent parent cannot be found safely here.
func removeOrphanedMaterialStages(ctx context.Context, target *os.Root, parent string) error {
	root, err := openPlainMaterialDirectory(target, parent)
	if err != nil {
		return err
	}
	return errors.Join(scanMaterialStages(ctx, root, false), root.Close())
}

func removeOrphanedMaterialPackageStages(ctx context.Context, target *os.Root) error {
	root, err := openPlainMaterialDirectory(target, "catalog-packages")
	if err != nil {
		return err
	}
	return errors.Join(scanMaterialStages(ctx, root, true), root.Close())
}

func openPlainMaterialDirectory(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("material consumer parent is not a plain directory")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	after, afterErr := parent.Lstat(name)
	if err != nil || afterErr != nil || !os.SameFile(before, opened) || !os.SameFile(before, after) || after.Mode()&os.ModeSymlink != 0 {
		return nil, errors.Join(errors.New("material consumer parent changed before cleanup"), err, afterErr, root.Close())
	}
	return root, nil
}

// ReadDir batches keep memory bounded even when the managed directory has
// many unrelated entries. Only names this installer generates are removed.
func scanMaterialStages(ctx context.Context, root *os.Root, packages bool) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	removed := false
	defer func() {
		if removed {
			result = errors.Join(result, dir.Sync())
		}
		result = errors.Join(result, dir.Close())
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			if packages {
				suffix := strings.TrimPrefix(name, ".material-package-")
				if !entry.IsDir() || suffix == name || len(suffix) == 0 || len(suffix) > 10 || strings.Trim(suffix, "0123456789") != "" {
					continue
				}
				if err := root.RemoveAll(name); err != nil {
					return err
				}
			} else {
				suffix := strings.TrimPrefix(name, ".material-")
				if !entry.Type().IsRegular() || suffix == name || len(suffix) != 32 || strings.Trim(suffix, "0123456789abcdef") != "" {
					continue
				}
				if err := root.Remove(name); err != nil {
					return err
				}
			}
			removed = true
		}
		if readErr == io.EOF {
			return ctx.Err()
		}
		if readErr != nil {
			return readErr
		}
	}
}

func ensureMaterialParent(root *os.Root, name string) error {
	parent := path.Dir(name)
	if parent == "." {
		return nil
	}
	current := "."
	for _, part := range strings.Split(parent, "/") {
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = root.Lstat(current)
			if err == nil {
				err = syncRootDir(root, path.Dir(current))
			}
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("material destination parent is not a plain directory")
		}
	}
	return nil
}

func verifyRootMaterial(ctx context.Context, root *os.Root, name, digest string, size int64) error {
	started := candidateProofStart(ctx)
	verifiedBytes := int64(0)
	defer func() { candidateProofObserve(ctx, proofLocalHash, started, verifiedBytes) }()
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() != size {
		return errors.New("installed material conflicts with the manifest")
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return errors.New("installed material changed before reading")
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	count, err := io.CopyBuffer(hash, materialContextReader{ctx, io.LimitReader(file, size+1)}, buffer)
	if err != nil || count != size || hex.EncodeToString(hash.Sum(nil)) != digest || ctx.Err() != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("installed material conflicts with the manifest")
	}
	after, err := root.Lstat(name)
	if err != nil || !os.SameFile(opened, after) {
		return errors.New("installed material changed after reading")
	}
	verifiedBytes = count
	return nil
}

func syncRootDir(root *os.Root, name string) error {
	dir, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// An interrupted or failed stage supplies no progress and no receipt.
func completedMaterialInstallationStage(ctx context.Context, progress func() error) error {
	if progress == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(progress(), ctx.Err())
}
