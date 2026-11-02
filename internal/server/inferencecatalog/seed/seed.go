package seed

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
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

// CatalogWithDigest returns the built-in catalog and its immutable package
// identity. Persistence belongs to the caller's catalog projection boundary.
func CatalogWithDigest() (inferencecatalog.FlatCatalog, string, error) {
	pkg, err := embeddedPackage()
	if err != nil {
		return inferencecatalog.FlatCatalog{}, "", err
	}
	root, err := embeddedPackageFS()
	if err != nil {
		return inferencecatalog.FlatCatalog{}, "", err
	}
	digest, err := inferencecatalog.PackageDigest(root)
	if err != nil {
		return inferencecatalog.FlatCatalog{}, "", fmt.Errorf("digest embedded inference catalog: %w", err)
	}
	return pkg.Catalog, digest, nil
}
