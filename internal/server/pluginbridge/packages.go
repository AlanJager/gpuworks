package pluginbridge

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// stopWait bounds a plugin's graceful stop (shutdown, SIGTERM, SIGKILL take
// at most 15 s under the default timing).
const stopWait = 20 * time.Second

// statePackagesLocked is every installed package as pushed state knows it.
// A package that no longer verifies, or whose build or certificate is
// revoked, is installed but not verified: its restrictions and banners stay
// enforced and only its grants are dropped.
func (b *Bridge) statePackagesLocked() []pushstate.Package {
	out := make([]pushstate.Package, 0, len(b.entries))
	for _, id := range slices.Sorted(maps.Keys(b.entries)) {
		e := b.entries[id]
		p := pushstate.Package{ID: id}
		if v := e.Verified; v != nil {
			p.Publisher, p.Grants, p.Features = v.Manifest.Publisher, v.Grants, v.Manifest.Features
			p.Verified = v.Revoked == nil
		}
		out = append(out, p)
	}
	return out
}

// refresh verifies the installed packages again and hands pushed state the
// new set.
func (b *Bridge) refresh() error {
	entries, err := b.pkgs.List()
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.entries = make(map[string]extpkg.Entry, len(entries))
	for _, e := range entries {
		b.entries[e.ID] = e
	}
	pkgs := b.statePackagesLocked()
	b.mu.Unlock()
	b.state.SetPackages(pkgs)
	return nil
}

// runnable reports whether an entry's server part may run now: verified,
// not revoked, enabled, with a resident server part. On-demand server parts
// are started by their first call, which nothing makes yet.
func runnable(e extpkg.Entry) bool {
	v := e.Verified
	return e.Err == nil && v != nil && v.Revoked == nil && e.Control.Enabled && v.Binary != "" &&
		v.Manifest.Runtime != nil && v.Manifest.Runtime.Lifecycle == extapi.LifecycleResident
}

// startIfEnabled starts package id's server part when it may run and Run
// is running: verified again first (only changed files are rehashed), then
// handed to the plugin host, which does not wait for it. The caller holds
// admin.
func (b *Bridge) startIfEnabled(id string) {
	b.mu.Lock()
	e, ok := b.entries[id]
	running := b.running
	b.mu.Unlock()
	if !ok || !running || !runnable(e) {
		return
	}
	v, err := b.pkgs.Recheck(id, false)
	if err != nil {
		b.log.Warn("plugin no longer verifies; it is not started", "plugin", id, "err", err)
		_ = b.refresh()
		return
	}
	spec, err := b.specOf(v)
	if err != nil {
		b.log.Error("plugin cannot be started", "plugin", id, "err", err)
		return
	}
	b.mu.Lock()
	b.pushers[id] = identity{version: v.Manifest.Version, pusher: pusherOf(v)}
	b.mu.Unlock()
	if err := b.host.Start(spec); err != nil {
		b.log.Error("plugin cannot be started", "plugin", id, "err", err)
	}
}

// stop stops package id's server part, if it runs, and forgets its
// identity. The caller holds admin.
func (b *Bridge) stop(ctx context.Context, id string) error {
	err := b.host.Stop(ctx, id)
	b.mu.Lock()
	delete(b.pushers, id)
	b.mu.Unlock()
	if errors.Is(err, pluginhost.ErrUnknownPlugin) {
		return nil
	}
	return err
}

// specOf is the plugin host's Spec for a verified package: its binary and
// directories, what its administrator approved, and the capabilities it may
// use (each required one at the version the server offers, each optional
// one at most at the version the package names).
func (b *Bridge) specOf(v *extpkg.Verified) (pluginhost.Spec, error) {
	m := v.Manifest
	dir := b.pkgs.Dir.Version(m.ID, m.Version)
	data, tmp := b.pkgs.Dir.Data(m.ID), b.pkgs.Dir.Tmp(m.ID)
	for _, d := range []string{data, tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return pluginhost.Spec{}, err
		}
	}
	offered := extpkg.Capabilities()
	approved := map[string]int{}
	for name := range m.Requires {
		approved[name] = offered[name]
	}
	for name, v := range m.Uses {
		if o, ok := offered[name]; ok {
			approved[name] = min(v, o)
		}
	}
	rt := m.Runtime
	return pluginhost.Spec{
		ID: m.ID, Version: m.Version, ManifestSHA256: v.Digest,
		Exe: filepath.Join(dir, filepath.FromSlash(v.Binary)), PackageDir: dir, DataDir: data, TmpDir: tmp,
		Lifecycle: rt.Lifecycle, Grant: grantOf(v), Approved: approved,
		MaxConcurrency: rt.MaxConcurrency, MemMiB: rt.MemMiB, Devices: rt.Devices,
		RequireLandlock: b.cfg.RequireIsolation == extapi.IsolationLandlock ||
			(v.Grants.MinIsolation == extapi.IsolationLandlock && extpkg.HostIsolation() == extapi.IsolationLandlock),
	}, nil
}

// grantOf is what the server approved for a package, as initialize tells
// it: the permissions its administrator approved (by the approval hash),
// the roles its certificate grants, and the namespaces it is the authority
// for.
func grantOf(v *extpkg.Verified) extapi.PluginGrant {
	m := v.Manifest
	g := extapi.PluginGrant{ID: m.ID, Version: m.Version, PermissionsGranted: slices.Clone(m.Permissions)}
	for _, r := range m.Roles {
		if slices.Contains(v.Grants.Roles, r) {
			g.RolesGranted = append(g.RolesGranted, r)
		}
	}
	if v.Grants.AuthorityID == m.ID {
		g.AuthorityNamespaces = slices.Clone(v.Grants.FeatureNS)
	}
	return g
}

// pusherOf is the identity a verified package's pushes are checked as.
// Generation is the pushing connection's, set per push.
func pusherOf(v *extpkg.Verified) pushstate.Pusher {
	m := v.Manifest
	return pushstate.Pusher{ID: m.ID, Publisher: m.Publisher, Grants: v.Grants,
		Permissions: slices.Clone(m.Permissions), Codes: slices.Clone(m.Codes)}
}
