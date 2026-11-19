package pluginbridge

import (
	"gpuworks.ai/gpuworks/internal/server/extpkg"
)

// recoverUpgrades finishes the upgrades a stopped server left in progress
// (their marker, <id>/upgrade.json, still exists), before any package is
// verified, loaded into pushed state or started: an upgrade stopped before
// the new version became current only loses its marker; any other is
// rolled back to the version it came from, with that version's data from
// the snapshot when the snapshot was taken. Each decision is logged with
// audit=true. A rollback that fails keeps the marker, is logged as an
// error, and is tried again at the next boot.
func (b *Bridge) recoverUpgrades() {
	ids, err := b.pkgs.Dir.Upgrading()
	if err != nil {
		b.log.Error("plugin upgrade markers cannot be read; an interrupted upgrade is not rolled back", "err", err)
		return
	}
	for _, id := range ids {
		b.recoverUpgrade(id)
	}
}

func (b *Bridge) recoverUpgrade(id string) {
	u, ok, err := b.pkgs.Dir.ReadUpgrade(id)
	if err != nil || !ok {
		b.log.Error("plugin upgrade marker cannot be read; the upgrade is not rolled back", "plugin", id, "err", err)
		return
	}
	current, err := b.pkgs.Dir.Current(id)
	if err != nil {
		b.log.Error("plugin upgrade not rolled back: the current version cannot be read", "plugin", id, "err", err)
		return
	}
	_, restore, err := b.pkgs.Dir.FindSnapshot(id, u.Snapshot)
	if err != nil {
		b.log.Error("plugin upgrade not rolled back: its snapshot cannot be read", "plugin", id, "snapshot", u.Snapshot, "err", err)
		return
	}
	if current == "" || (current == u.From && (u.From != u.To || !restore)) {
		// Not installed any more, or the new version never became
		// current: its data was never touched.
		err := b.pkgs.FinishUpgrade(id)
		b.log.Info("plugin upgrade interrupted before the new version became current; nothing to roll back", "audit", true,
			"plugin", id, "from", u.From, "to", u.To, "current", current, "err", err)
		return
	}
	snapshot := u.Snapshot
	if !restore {
		snapshot = ""
	}
	if _, err := b.pkgs.Rollback(id, extpkg.RollbackOptions{To: u.From, Restore: restore, Snapshot: snapshot, By: "boot recovery"}); err != nil {
		code, _, _ := codeOf(err)
		b.log.Error("plugin upgrade interrupted and not rolled back; the next boot tries again", "audit", true, "plugin", id,
			"from", u.From, "to", u.To, "current", current, "code", code, "err", err)
		return
	}
	b.log.Warn("plugin upgrade interrupted; rolled back at boot", "audit", true, "plugin", id, "from", u.From, "to", u.To,
		"current", current, "snapshot", snapshot, "restored", restore, "started", u.Started)
}
