package pluginbridge

import (
	"maps"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
)

// declareFeatures makes the feature keys of the installed packages, as
// pushed state answers for them, the server's dynamic declarations, and
// announces the change. A key a compiled module declares stays the
// module's: install refuses such a package, so only one installed before
// that check, or placed on disk by hand, can declare one, and its
// declaration is left out (logged at boot). It runs at Open and after every
// refresh, under admin.
func (b *Bridge) declareFeatures() {
	d := b.cfg.Features
	if d == nil {
		return
	}
	features := b.state.Features()
	keep := features[:0]
	for _, f := range features {
		if _, static := b.cfg.StaticFeatures[f.Key]; !static {
			keep = append(keep, f)
		}
	}
	if err := d.Replace(keep, b.state); err != nil {
		// The keys come from pushed state's registry, which declares
		// each valid key once, and the static ones are left out above.
		b.log.Error("plugin feature keys cannot be declared", "err", err)
	}
	if announce := b.cfg.State.Effects.FeaturesChanged; announce != nil {
		announce()
	}
}

// logFeatureClashes logs, at boot, each installed package's feature key
// that someone else keeps: a compiled module, or another package that
// declared it first (the lower id, the owner it has had since both were
// installed). Nothing changes owner; a package installed now that clashes
// is refused (PLUGIN_FEATURE_TAKEN).
func (b *Bridge) logFeatureClashes() {
	b.mu.Lock()
	entries := maps.Clone(b.entries)
	b.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(entries)) {
		v := entries[id].Verified
		if v == nil {
			continue
		}
		for _, f := range v.Manifest.Features {
			if owner := b.FeatureOwner(id, f.Key); owner != "" {
				b.log.Warn("installed plugin declares a feature key another owner keeps; the owner keeps it", "plugin", id, "feature", f.Key, "owner", owner)
			}
		}
	}
}

// FeatureOwner is who, other than package id, provides feature key: the
// compiled module that declares it, or the installed package pushed state
// answers for it with; "" when no one does (extpkg.FeatureOwners).
func (b *Bridge) FeatureOwner(id, key string) string {
	k := featuregate.Key(key)
	if module, ok := b.cfg.StaticFeatures[k]; ok {
		return module
	}
	if owner := b.state.FeatureOwner(k); owner != id {
		return owner
	}
	return ""
}

var _ extpkg.FeatureOwners = (*Bridge)(nil)
