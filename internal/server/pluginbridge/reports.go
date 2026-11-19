package pluginbridge

import (
	"fmt"
	"os"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The administrator's reports on one package: what pushed state holds of
// it, its logs, and what its uninstall would change. Each reads memory
// only.

// Report is what pushed state holds for package id and how it stands. It
// answers for an uninstalled package whose rows are still held (an
// uninstall that failed half way); any other id that is not installed is
// PLUGIN_NOT_INSTALLED.
func (b *Bridge) Report(id string) (pushstate.OwnerReport, error) {
	if !b.installedOrHeld(id) {
		return pushstate.OwnerReport{}, notInstalled(id)
	}
	return b.state.OwnerReport(id), nil
}

// Logs are package id's supervision events and stderr output after from,
// across its processes and restarts, with a channel closed at its next
// event or output (nil, never closed, when it has not started since the
// server started). An id that is not installed is PLUGIN_NOT_INSTALLED.
func (b *Bridge) Logs(id string, from pluginhost.LogCursor) (pluginhost.Logs, <-chan struct{}, error) {
	if _, err := b.entry(id); err != nil {
		return pluginhost.Logs{}, nil, err
	}
	changed := b.host.Changed(id)
	logs, _ := b.host.Logs(id, from)
	return logs, changed, nil
}

// Impact is what uninstalling package id would change now; Uninstall runs
// only when it is confirmed by this digest. It answers for an uninstalled
// package whose rows are still held, or whose data an uninstall without
// purge kept (for the purge that removes it); any other id that is not
// installed is PLUGIN_NOT_INSTALLED.
func (b *Bridge) Impact(id string) (pushstate.Impact, error) {
	defer b.locks.lock(id)()
	return b.impactLocked(id)
}

// impactLocked is Impact with the lock of id held, so no install, upgrade or
// uninstall changes the package between the preview and the uninstall
// that compares it.
func (b *Bridge) impactLocked(id string) (pushstate.Impact, error) {
	if !b.installedOrHeld(id) && !b.dataKept(id) {
		return pushstate.Impact{}, notInstalled(id)
	}
	version := ""
	if e, err := b.entry(id); err == nil {
		version = e.Version
	}
	return b.state.UninstallImpact(id, version)
}

// installedOrHeld reports whether id is installed or pushed state still
// holds rows of it.
func (b *Bridge) installedOrHeld(id string) bool {
	if !extapi.ValidPackageID(id) {
		return false
	}
	if _, err := b.entry(id); err == nil {
		return true
	}
	return len(b.state.Held(id)) > 0
}

// confirmRequired refuses an uninstall that was not confirmed with the
// impact's digest: its params count what it changes and carry the digest
// to confirm with.
func confirmRequired(im pushstate.Impact, given extapi.Digest) error {
	text := fmt.Sprintf("uninstalling %s releases %d held nodes, lifts %d caps and locks %d features; confirm with %s",
		im.Owner, len(im.Nodes), len(im.Caps), len(im.Features), im.Digest)
	if given != "" {
		text += " (the impact changed since it was previewed)"
	}
	return messages.NewProductError(messages.ErrorPluginUninstallConfirmRequired, text, map[string]any{
		"id": im.Owner, "confirm": string(im.Digest), "held_nodes": len(im.Nodes), "users_affected": im.UsersAffected,
		"caps_lifted": len(im.Caps), "features_locked": len(im.Features), "banners": im.Banners, "overrides": len(im.Overrides),
	})
}

// dataKept reports whether the directory of package id is still there
// though it is not installed: its data, which an uninstall without purge
// keeps.
func (b *Bridge) dataKept(id string) bool {
	if !extapi.ValidPackageID(id) {
		return false
	}
	_, err := os.Lstat(b.pkgs.Dir.Package(id))
	return err == nil
}
