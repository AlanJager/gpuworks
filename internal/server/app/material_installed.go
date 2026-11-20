//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
)

// checkInstalledMaterialManifest is a read-only check of the receiver's
// business paths. The caller separately verifies current membership, the
// manifest pointer, and all content-addressed replicas before and after it.
// A valid replica by itself is not proof that its consumer can read the file.
func checkInstalledMaterialManifest(ctx context.Context, receiver *materialReceiver, manifest materialManifest) (result error) {
	return checkInstalledMaterialManifestWithProgress(ctx, receiver, manifest, nil)
}

func checkInstalledMaterialManifestWithProgress(ctx context.Context, receiver *materialReceiver, manifest materialManifest, progress func() error) (result error) {
	if receiver == nil || filepath.Base(receiver.root) != "material-replicas" {
		return errors.New("installed material check requires a managed receiver")
	}
	dataDir := filepath.Dir(receiver.root)
	target, err := os.OpenRoot(dataDir)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, target.Close(), ctx.Err()) }()
	if err := checkMaterialDataRoot(dataDir, target); err != nil {
		return err
	}
	var catalog *os.Root
	var catalogIdentity os.FileInfo
	checkCatalog := func() error {
		return errors.Join(checkMaterialDataRoot(dataDir, target), checkMaterialOwnedDirectory(target, "catalog-packages", catalogIdentity))
	}
	checkedPackages := make(map[string]bool)
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validMaterialConsumerPath(file.Class, file.Path) || !validMaterialDigest(file.SHA256) || file.Bytes <= 0 || file.Bytes > maxMaterialReplicaBytes {
			return errors.New("installed material check received an invalid manifest entry")
		}
		switch file.Class {
		case MaterialMemberIdentity:
			// Member identity belongs to this receiver, not to the source.
			continue
		case MaterialCatalogPackage, MaterialRuntimeAsset:
			parts := strings.SplitN(file.Path, "/", 3)
			if len(parts) != 3 || !validMaterialDigest(parts[1]) {
				return errors.New("installed catalog path has no package digest")
			}
			if catalog == nil {
				catalog, err = openPlainMaterialDirectory(target, "catalog-packages")
				if err != nil {
					return err
				}
				defer func() { result = errors.Join(result, catalog.Close()) }()
				catalogIdentity, err = catalog.Stat(".")
				if err != nil {
					return err
				}
			}
			if err := checkCatalog(); err != nil {
				return err
			}
			if err := checkMaterialExistingParent(target, file.Path); err != nil {
				return err
			}
			if err := verifyRootMaterial(ctx, target, file.Path, file.SHA256, file.Bytes); err != nil {
				return fmt.Errorf("installed %s material %s differs: %w", file.Class, file.Path, err)
			}
			if checkedPackages[parts[1]] {
				if err := completedMaterialInstallationStage(ctx, progress); err != nil {
					return err
				}
				continue
			}
			if _, err := packages.LoadRootContext(ctx, catalog, parts[1]); err != nil {
				return fmt.Errorf("installed catalog package %s differs: %w", parts[1], err)
			}
			if err := checkCatalog(); err != nil {
				return err
			}
			checkedPackages[parts[1]] = true
		case MaterialMasterSecret, MaterialOfflineImage, MaterialBenchmarkInput,
			MaterialBenchmarkDetail, MaterialModelLogo, MaterialSSHHostKeys, MaterialAgentOverride:
			if err := checkMaterialExistingParent(target, file.Path); err != nil {
				return err
			}
			if err := verifyRootMaterial(ctx, target, file.Path, file.SHA256, file.Bytes); err != nil {
				return fmt.Errorf("installed %s material %s differs: %w", file.Class, file.Path, err)
			}
		default:
			return fmt.Errorf("unknown installed material class %q", file.Class)
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
	if catalog != nil {
		if err := checkCatalog(); err != nil {
			return err
		}
	}
	return errors.Join(ctx.Err(), checkMaterialDataRoot(dataDir, target))
}

func checkMaterialExistingParent(root *os.Root, name string) error {
	current := "."
	for _, part := range strings.Split(path.Dir(name), "/") {
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("installed material parent is not a plain directory")
		}
	}
	return nil
}
