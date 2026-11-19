package pluginbridge

import (
	"maps"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/extpkg/catalog"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// CatalogEntry is one package the imported indexes list or this server
// has installed, with the server's verdict on what to install.
type CatalogEntry struct {
	ID, Publisher string
	// Installed is the installed version; empty when it is not installed.
	Installed string
	// Best is the newest build this server can install; nil when the
	// indexes list none it can (or the package is only installed).
	Best *extapi.IndexEntry
	// Installable reports that Best is not installed yet: the package is
	// not installed, or Best is newer than the installed version.
	Installable bool
	// Refused are the builds newer than Best this server will not install,
	// newest first, each with its coded reason.
	Refused []CatalogRefusal
}

// CatalogRefusal is a build the server will not install, and why.
type CatalogRefusal struct {
	Version, MinCoreHint string
	Code                 string
	Params               map[string]any
	Message              string
}

// CatalogIndexError is an imported index that no longer verifies, left out
// of the catalog.
type CatalogIndexError struct {
	Publisher, Code string
	Params          map[string]any
	Message         string
}

// Catalog is the packages the imported indexes offer joined with the
// installed ones, sorted by id.
type Catalog struct {
	Entries []CatalogEntry
	// Errors are the indexes left out, by publisher.
	Errors []CatalogIndexError
}

// Catalog verifies every imported index again (catalog.Load), decides per
// package which build this server installs (catalog.Choose) and joins the
// result with the installed packages; it also refreshes what the views
// report as pending (View.Pending).
func (b *Bridge) Catalog() (Catalog, error) {
	indexes, errs, err := catalog.Load(b.pkgs.Verifier.Trust, b.pkgs.Dir)
	if err != nil {
		return Catalog{}, err
	}
	b.mu.Lock()
	installed := map[string]string{}
	publishers := map[string]string{}
	for id, e := range b.entries {
		installed[id] = e.Version
		if e.Verified != nil {
			publishers[id] = e.Verified.Manifest.Publisher
		}
	}
	b.mu.Unlock()

	byID := map[string]CatalogEntry{}
	for _, x := range indexes {
		for _, c := range catalog.Choose(x, b.pkgs.Verifier.Offered, b.pkgs.Verifier.Trust) {
			if _, seen := byID[c.ID]; seen {
				// A package id belongs to one publisher; a second index
				// listing it cannot install it anyway.
				continue
			}
			e := CatalogEntry{ID: c.ID, Publisher: x.Publisher, Installed: installed[c.ID], Best: c.Best}
			for _, r := range c.Newer {
				code, params, text := codeOf(r.Err)
				e.Refused = append(e.Refused, CatalogRefusal{Version: r.Entry.Version, MinCoreHint: r.Entry.MinCoreHint, Code: code, Params: params, Message: text})
			}
			e.Installable = installable(e.Best, e.Installed)
			byID[c.ID] = e
		}
	}
	for id, v := range installed {
		if _, listed := byID[id]; !listed {
			byID[id] = CatalogEntry{ID: id, Publisher: publishers[id], Installed: v}
		}
	}
	out := Catalog{Entries: make([]CatalogEntry, 0, len(byID))}
	pending := map[string]string{}
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		e := byID[id]
		out.Entries = append(out.Entries, e)
		if e.Installable && e.Installed != "" {
			pending[id] = e.Best.Version
		}
	}
	for _, p := range slices.Sorted(maps.Keys(errs)) {
		code, params, text := codeOf(errs[p])
		out.Errors = append(out.Errors, CatalogIndexError{Publisher: p, Code: code, Params: params, Message: text})
	}
	b.mu.Lock()
	b.pending = pending
	b.mu.Unlock()
	return out, nil
}

// refreshPending reads the catalog again for View.Pending, at boot and
// after the installed packages change; an index that cannot be read leaves
// the last answer and is logged.
func (b *Bridge) refreshPending() {
	if b.pkgs.Verifier == nil || b.pkgs.Verifier.Trust == nil {
		return
	}
	if _, err := b.Catalog(); err != nil {
		b.log.Warn("plugin package indexes cannot be read; pending upgrades are not shown", "err", err)
	}
}

// installable reports whether best is a build to install over installed
// (empty when not installed).
func installable(best *extapi.IndexEntry, installed string) bool {
	if best == nil {
		return false
	}
	if installed == "" {
		return true
	}
	c, err := extapi.CompareVersions(best.Version, installed)
	return err == nil && c > 0
}

// previousOf is the newest unpacked version older than current, kept for
// rollback; empty when there is none.
func previousOf(current string, versions []string) string {
	prev := ""
	for _, v := range versions {
		if c, err := extapi.CompareVersions(v, current); err != nil || c >= 0 {
			continue
		}
		if prev == "" {
			prev = v
			continue
		}
		if c, err := extapi.CompareVersions(v, prev); err == nil && c > 0 {
			prev = v
		}
	}
	return prev
}
