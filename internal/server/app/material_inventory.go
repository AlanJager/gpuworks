//go:build linux && dqlite

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// MaterialClass identifies a source of durable application bytes. A consumer
// must account for every class, including classes with no current references.
type MaterialClass string

const (
	MaterialMasterSecret    MaterialClass = "master-secret"
	MaterialMemberIdentity  MaterialClass = "member-identity"
	MaterialCatalogPackage  MaterialClass = "catalog-package"
	MaterialRuntimeAsset    MaterialClass = "runtime-asset"
	MaterialOfflineImage    MaterialClass = "offline-image"
	MaterialBenchmarkInput  MaterialClass = "benchmark-input"
	MaterialBenchmarkDetail MaterialClass = "benchmark-detail"
	MaterialModelLogo       MaterialClass = "model-logo"
	MaterialSSHHostKeys     MaterialClass = "ssh-host-keys"
	MaterialAgentOverride   MaterialClass = "agent-override"
)

var requiredMaterialClasses = []MaterialClass{
	MaterialMasterSecret, MaterialMemberIdentity, MaterialCatalogPackage, MaterialRuntimeAsset,
	MaterialOfflineImage, MaterialBenchmarkInput, MaterialBenchmarkDetail,
	MaterialModelLogo, MaterialSSHHostKeys, MaterialAgentOverride,
}

// MaterialFile is a local source. Its digest describes bytes on this member;
// listing it does not prove that another member has received those bytes.
type MaterialFile struct {
	Class  MaterialClass
	Path   string
	SHA256 string
}

type MaterialInventory struct {
	Classes []MaterialClass
	Files   []MaterialFile
}

// CheckCoverage rejects a transfer or archive plan that omitted a required
// class. Empty classes must still be explicitly covered, since a later write
// can add a reference without changing the transfer implementation.
func (i MaterialInventory) CheckCoverage(covered []MaterialClass) error {
	known := make(map[MaterialClass]bool, len(requiredMaterialClasses))
	for _, class := range requiredMaterialClasses {
		known[class] = true
	}
	seen := make(map[MaterialClass]bool, len(covered))
	for _, class := range covered {
		if !known[class] || seen[class] {
			return fmt.Errorf("invalid material coverage class %q", class)
		}
		seen[class] = true
	}
	for _, class := range i.Classes {
		if !known[class] || !seen[class] {
			return fmt.Errorf("required material class %q is not covered", class)
		}
	}
	for _, class := range requiredMaterialClasses {
		if !seen[class] {
			return fmt.Errorf("required material class %q is not covered", class)
		}
	}
	return nil
}

// InspectRequiredMaterials reads database references and the managed data
// directory. Missing or unreadable referenced bytes fail the entire scan;
// callers must not treat a partial result as a replication or backup plan.
func InspectRequiredMaterials(ctx context.Context, dataDir string, st *store.Store) (MaterialInventory, error) {
	if st == nil || dataDir == "" {
		return MaterialInventory{}, errors.New("material inventory requires a store and data directory")
	}
	root, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return MaterialInventory{}, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return MaterialInventory{}, err
	}
	result := MaterialInventory{Classes: append([]MaterialClass(nil), requiredMaterialClasses...)}
	add := func(class MaterialClass, path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%s material is unavailable: %w", class, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s material is not a regular file", class)
		}
		digest, err := hashMaterialFile(ctx, path)
		if err != nil {
			return fmt.Errorf("read %s material: %w", class, err)
		}
		result.Files = append(result.Files, MaterialFile{Class: class, Path: path, SHA256: digest})
		return nil
	}
	addTree := func(class MaterialClass, path string) error {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s material root is not a directory", class)
		}
		return filepath.WalkDir(path, func(file string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if class == MaterialMemberIdentity && (strings.HasSuffix(entry.Name(), ".lock") || entry.Name() == ".pending") {
				return nil
			}
			// The policy writer's temporary bytes are not published identity.
			if class == MaterialMemberIdentity && filepath.Dir(file) == filepath.Join(root, "gpuworks.db.dqlite", ".transport") && strings.HasPrefix(entry.Name(), ".transport-policy-") {
				return nil
			}
			return add(class, file)
		})
	}
	if err := add(MaterialMasterSecret, filepath.Join(root, "master.key")); err != nil {
		return MaterialInventory{}, err
	}
	// The seed and joined member use different identity layouts. Both are
	// necessary when present; a cluster identity with neither is incomplete.
	identityRoots := []string{filepath.Join(root, enrollmentDirectory), filepath.Join(root, "gpuworks.db.join", "materials"), filepath.Join(root, "gpuworks.db.dqlite", ".transport")}
	identityPresent := false
	for _, path := range identityRoots {
		if _, err := os.Lstat(path); err == nil {
			identityPresent = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return MaterialInventory{}, err
		}
		if err := addTree(MaterialMemberIdentity, path); err != nil {
			return MaterialInventory{}, err
		}
	}
	for _, name := range []string{"member.key", "config.json", "materials.commit"} {
		path := filepath.Join(root, "gpuworks.db.join", name)
		if _, err := os.Lstat(path); err == nil {
			identityPresent = true
			if err := add(MaterialMemberIdentity, path); err != nil {
				return MaterialInventory{}, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return MaterialInventory{}, err
		}
	}
	if _, err := st.ClusterIdentity(ctx); err == nil && !identityPresent {
		return MaterialInventory{}, errors.New("cluster identity has no local identity materials")
	} else if err != nil && !errors.Is(err, store.ErrClusterNotInitialized) {
		return MaterialInventory{}, err
	}
	if err := ctx.Err(); err != nil {
		return MaterialInventory{}, err
	}
	digests, err := st.CompleteCatalogPackageReferences(ctx)
	if err != nil {
		return MaterialInventory{}, err
	}
	packageRoot := filepath.Join(root, "catalog-packages")
	var cache *packages.Store
	if len(digests) != 0 {
		if info, err := os.Lstat(packageRoot); err != nil || !info.IsDir() {
			return MaterialInventory{}, errors.New("referenced catalog package root is unavailable")
		}
		cache, err = packages.New(packageRoot)
		if err != nil {
			return MaterialInventory{}, err
		}
	}
	for _, digest := range digests {
		if _, err := cache.LoadContext(ctx, digest); err != nil {
			return MaterialInventory{}, fmt.Errorf("catalog package %s is unavailable: %w", digest, err)
		}
		packageDir := filepath.Join(packageRoot, digest)
		if err := addTree(MaterialCatalogPackage, packageDir); err != nil {
			return MaterialInventory{}, err
		}
		// Runtime assets live inside package digests, but are a separate
		// consumer contract: an archive or receiver cannot silently drop them.
		if err := filepath.WalkDir(packageDir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			rel, err := filepath.Rel(packageDir, path)
			if err != nil {
				return err
			}
			if strings.Contains(filepath.ToSlash(rel), "/assets/") {
				return add(MaterialRuntimeAsset, path)
			}
			return nil
		}); err != nil {
			return MaterialInventory{}, err
		}
	}
	images, err := st.OfflineImageBlobRows(ctx)
	if err != nil {
		return MaterialInventory{}, err
	}
	seenImages := make(map[string]MaterialFile, len(images))
	for _, image := range images {
		path, err := containedMaterialPath(filepath.Join(root, "images"), image.OfflineBlobPath)
		if err != nil {
			return MaterialInventory{}, err
		}
		if !validMaterialDigest(image.OfflineBlobSHA256) || image.OfflineBlobBytes <= 0 {
			return MaterialInventory{}, errors.New("offline image material has no valid catalog digest and size")
		}
		file, exists := seenImages[path]
		if !exists {
			if err := add(MaterialOfflineImage, path); err != nil {
				return MaterialInventory{}, err
			}
			file = result.Files[len(result.Files)-1]
			seenImages[path] = file
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != image.OfflineBlobBytes || file.SHA256 != image.OfflineBlobSHA256 {
			return MaterialInventory{}, errors.New("offline image material differs from the current catalog record")
		}
	}
	runs, err := st.BenchmarkTokenizerRows(ctx)
	if err != nil {
		return MaterialInventory{}, err
	}
	artifactRoot := filepath.Join(root, "benchmark-artifacts")
	for _, run := range runs {
		if strings.HasPrefix(run.TokenizerRef, "artifact://") {
			name := strings.TrimPrefix(run.TokenizerRef, "artifact://")
			if filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") || !strings.HasPrefix(name, "tokenizer-") || !strings.HasSuffix(name, ".json") {
				return MaterialInventory{}, errors.New("invalid uploaded tokenizer reference")
			}
			path, err := containedMaterialPath(artifactRoot, name)
			if err != nil {
				return MaterialInventory{}, err
			}
			if err := add(MaterialBenchmarkInput, path); err != nil {
				return MaterialInventory{}, err
			}
			continue
		}
		if strings.HasPrefix(run.TokenizerRef, "hf://") || strings.HasPrefix(run.TokenizerRef, "ms://") {
			continue
		}
		if reference, err := url.Parse(run.TokenizerRef); err == nil && (reference.Scheme == "http" || reference.Scheme == "https") {
			continue
		}
		path, err := containedMaterialPath(artifactRoot, run.TokenizerRef)
		if err != nil {
			return MaterialInventory{}, err
		}
		if err := add(MaterialBenchmarkInput, path); err != nil {
			return MaterialInventory{}, err
		}
	}
	// Upload returns a path before any benchmark run references it. Preserve
	// these accepted inputs as well; duplicate paths are removed below.
	if entries, err := os.ReadDir(artifactRoot); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "tokenizer-") && strings.HasSuffix(entry.Name(), ".json") {
				if err := add(MaterialBenchmarkInput, filepath.Join(artifactRoot, entry.Name())); err != nil {
					return MaterialInventory{}, err
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return MaterialInventory{}, err
	}
	points, err := st.BenchmarkDetailRows(ctx)
	if err != nil {
		return MaterialInventory{}, err
	}
	for _, point := range points {
		path, err := containedMaterialPath(artifactRoot, point.DetailPath)
		if err != nil {
			return MaterialInventory{}, err
		}
		if err := add(MaterialBenchmarkDetail, path); err != nil {
			return MaterialInventory{}, err
		}
	}
	models, err := st.ModelLogoRows(ctx)
	if err != nil {
		return MaterialInventory{}, err
	}
	for _, model := range models {
		path, err := containedMaterialPath(filepath.Join(root, "model-logos"), model.LogoPath)
		if err != nil {
			return MaterialInventory{}, err
		}
		if err := add(MaterialModelLogo, path); err != nil {
			return MaterialInventory{}, err
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "ssh-hostkeys.json")); err == nil {
		if err := add(MaterialSSHHostKeys, filepath.Join(root, "ssh-hostkeys.json")); err != nil {
			return MaterialInventory{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return MaterialInventory{}, err
	}
	if err := addTree(MaterialAgentOverride, filepath.Join(root, "dl")); err != nil {
		return MaterialInventory{}, err
	}
	sort.Slice(result.Files, func(a, b int) bool {
		if result.Files[a].Class != result.Files[b].Class {
			return result.Files[a].Class < result.Files[b].Class
		}
		return result.Files[a].Path < result.Files[b].Path
	})
	files := result.Files[:0]
	for _, file := range result.Files {
		if len(files) == 0 || files[len(files)-1].Class != file.Class || files[len(files)-1].Path != file.Path {
			files = append(files, file)
		}
	}
	result.Files = files
	return result, nil
}

func hashMaterialFile(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return hashMaterialReader(ctx, file)
}

func hashMaterialReader(ctx context.Context, reader io.Reader) (string, error) {
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if errors.Is(err, io.EOF) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return hex.EncodeToString(hash.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}

func containedMaterialPath(root, ref string) (string, error) {
	if ref == "" {
		return "", errors.New("empty material reference")
	}
	path := ref
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("material reference escapes its managed directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if resolved != path {
		return "", errors.New("material reference traverses a symbolic link")
	}
	return path, nil
}
