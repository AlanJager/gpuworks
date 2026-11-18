// Package compat answers whether this server build may run on a data
// directory that a newer build may have written (ADR 0071, decision 16: a
// downgrade is detected, never migrated). Persisted plugin state carries
// version markers (extapi.Marker): the core's own, and one per package's
// data. A core marker newer than this build knows refuses the start with
// PLUGIN_STATE_TOO_NEW; a package's data marker newer than the installed
// build's storage.state_version, or a capability the build requires and
// this server lacks, keeps that package from running. Check reports all of
// it without changing anything, for `gpuworks plugin compat`.
package compat

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/extpkg/archive"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// coreMarkers are the format versions of the core's own persisted plugin
// state that this build reads, by marker name. The part of the core that
// persists a kind of state adds its marker here in the change that writes
// it; a version only ever rises. Pushed state writes "pstate.<kind>" at
// version 1 for every table kind (package pushstate, Markers; a test keeps
// the two equal).
var coreMarkers = pushedStateMarkers()

func pushedStateMarkers() map[string]int {
	out := map[string]int{}
	for _, kind := range extapi.TableKinds() {
		out["pstate."+kind] = 1
	}
	return out
}

// CoreMarkers returns the core markers this build reads.
func CoreMarkers() map[string]int { return maps.Clone(coreMarkers) }

// CheckStartup refuses to start a server whose database holds a core
// marker newer than this build reads (extapi.ErrorStateTooNew, naming the
// server version to run instead). Package data markers are left to the
// check before each package starts.
func CheckStartup(stored []extapi.Marker) error {
	return extapi.CheckMarkers(stored, CoreMarkers())
}

// Package is what Check needs of one installed package.
type Package struct {
	ID      string
	Version string
	// Manifest is the installed manifest; nil when ManifestErr says why it
	// could not be read.
	Manifest    *extapi.Manifest
	ManifestErr error
}

// Finding is one reason this build cannot run something as it is.
type Finding struct {
	// Subject is a marker name, or the id of a package.
	Subject string
	Code    string
	Params  map[string]any
	Message string
}

// Report is the outcome of Check.
type Report struct {
	// Blocking findings stop the server from starting.
	Blocking []Finding
	// Packages findings keep one package from running.
	Packages []Finding
}

// OK reports whether nothing would stop or be refused.
func (r Report) OK() bool { return len(r.Blocking) == 0 && len(r.Packages) == 0 }

// Check compares the stored markers and the installed packages with this
// build: core markers against core, each package's data marker against its
// storage.state_version, and each package's required capabilities against
// offered.
func Check(stored []extapi.Marker, core, offered map[string]int, pkgs []Package) Report {
	var r Report
	for _, m := range extapi.TooNew(stored, core) {
		r.Blocking = append(r.Blocking, finding(m.Name, extapi.CheckMarkers([]extapi.Marker{m}, core)))
	}
	byName := map[string]extapi.Marker{}
	for _, m := range stored {
		byName[m.Name] = m
	}
	for _, p := range pkgs {
		if p.ManifestErr != nil {
			r.Packages = append(r.Packages, finding(p.ID, p.ManifestErr))
			continue
		}
		name := extapi.PluginDataMarker(p.ID)
		if m, ok := byName[name]; ok {
			sv := 0
			if p.Manifest.Storage != nil {
				sv = p.Manifest.Storage.StateVersion
			}
			if err := extapi.CheckMarkers([]extapi.Marker{m}, map[string]int{name: sv}); err != nil {
				r.Packages = append(r.Packages, finding(p.ID, err))
			}
		}
		caps := make([]string, 0, len(p.Manifest.Requires))
		for c := range p.Manifest.Requires {
			caps = append(caps, c)
		}
		sort.Strings(caps)
		for _, c := range caps {
			if want := p.Manifest.Requires[c]; offered[c] < want {
				r.Packages = append(r.Packages, Finding{Subject: p.ID, Code: messages.ErrorPluginIncompatible,
					Params:  map[string]any{"capability": c, "version": want, "offered": offered[c], "min_core_hint": p.Manifest.MinCoreHint},
					Message: p.ID + " requires " + c + " at a version this server does not offer"})
			}
		}
	}
	return r
}

func finding(subject string, err error) Finding {
	f := Finding{Subject: subject, Message: err.Error()}
	var xe *extapi.Error
	var pe *messages.ProductError
	switch {
	case errors.As(err, &xe):
		f.Code, f.Params = xe.Code, xe.Params
	case errors.As(err, &pe):
		f.Code, f.Params = pe.Code(), pe.Params()
	}
	return f
}

// Installed reads the manifest of the current version of every package
// installed in d, without verifying signatures: Check needs only what the
// packages declare.
func Installed(d layout.Dir) ([]Package, error) {
	ids, err := d.Installed()
	if err != nil {
		return nil, err
	}
	out := make([]Package, 0, len(ids))
	for _, id := range ids {
		v, err := d.Current(id)
		if err != nil {
			return nil, err
		}
		p := Package{ID: id, Version: v}
		raw, err := os.ReadFile(filepath.Join(d.Version(id, v), archive.ManifestMember))
		if err == nil {
			p.Manifest, err = extapi.DecodeManifest(raw)
		}
		p.ManifestErr = err
		out = append(out, p)
	}
	return out, nil
}
