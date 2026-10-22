package seed

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

//go:embed catalog
var embeddedCatalog embed.FS

// Catalog returns the validated catalog shipped by the server. The embedded
// multi-file package is the sole built-in authority; the database is only its
// queryable projection.
func Catalog() (inferencecatalog.FlatCatalog, error) {
	pkg, err := embeddedPackage()
	if err != nil {
		return inferencecatalog.FlatCatalog{}, err
	}
	return pkg.Catalog, nil
}

// Archive returns the deterministic archive of the embedded catalog package.
// Callers persist it in the content-addressed package store at startup.
func Archive() ([]byte, error) {
	root, err := embeddedPackageFS()
	if err != nil {
		return nil, err
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, root); err != nil {
		return nil, err
	}
	return archive.Bytes(), nil
}

// Published returns the embedded package as the distribution channel
// publishes it: the deterministic archive, its package digest (what upstream
// specs record) and its manifest (ADR 0040).
func Published() ([]byte, string, inferencecatalog.PackageManifest, error) {
	pkg, err := embeddedPackage()
	if err != nil {
		return nil, "", inferencecatalog.PackageManifest{}, err
	}
	root, err := embeddedPackageFS()
	if err != nil {
		return nil, "", inferencecatalog.PackageManifest{}, err
	}
	digest, err := inferencecatalog.PackageDigest(root)
	if err != nil {
		return nil, "", inferencecatalog.PackageManifest{}, fmt.Errorf("digest embedded inference catalog: %w", err)
	}
	archive, err := Archive()
	if err != nil {
		return nil, "", inferencecatalog.PackageManifest{}, err
	}
	return archive, digest, pkg.Manifest, nil
}

func embeddedPackage() (inferencecatalog.Package, error) {
	root, err := embeddedPackageFS()
	if err != nil {
		return inferencecatalog.Package{}, fmt.Errorf("open embedded inference catalog package: %w", err)
	}
	pkg, err := inferencecatalog.LoadPackage(root)
	if err != nil {
		return inferencecatalog.Package{}, fmt.Errorf("load embedded inference catalog package: %w", err)
	}
	return pkg, nil
}

func embeddedPackageFS() (fs.FS, error) {
	root, err := fs.Sub(embeddedCatalog, "catalog")
	if err != nil {
		return nil, fmt.Errorf("open embedded inference catalog package: %w", err)
	}
	return root, nil
}

// Seed reconciles only upstream catalog rows. Store reconciliation preserves
// every source=user spec created by cloning or import, including user specs
// attached to an original model that also has upstream specs.
func Seed(st *store.Store) error {
	if st == nil {
		return fmt.Errorf("seed inference catalog: store is nil")
	}
	pkg, err := embeddedPackage()
	if err != nil {
		return err
	}
	root, err := embeddedPackageFS()
	if err != nil {
		return err
	}
	digest, err := inferencecatalog.PackageDigest(root)
	if err != nil {
		return fmt.Errorf("digest embedded inference catalog: %w", err)
	}
	models, err := inferencecatalog.ToFlatStoreModels(pkg.Catalog)
	if err != nil {
		return fmt.Errorf("convert embedded inference catalog: %w", err)
	}
	for index := range models {
		models[index].PackageDigest = digest
		for specIndex := range models[index].Specs {
			models[index].Specs[specIndex].Source = "upstream"
			models[index].Specs[specIndex].PackageDigest = digest
		}
	}
	if err := st.ReconcileInferenceCatalog(models); err != nil {
		return fmt.Errorf("reconcile embedded inference catalog: %w", err)
	}
	return nil
}
