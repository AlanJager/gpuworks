package extpkg

import (
	"fmt"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// snapshot copies package id's data, recording its data marker, as
// snapshot name of version from. The caller has stopped the plugin and
// written its upgrade marker, which keeps Cleanup off the partial copy; it
// need not hold the directory's lock.
func (h *Host) snapshot(id, from, name string) (layout.Snapshot, error) {
	own, _, err := h.dataMarker(id)
	if err == nil {
		var s layout.Snapshot
		s, err = h.Dir.TakeSnapshot(id, layout.Snapshot{Name: name, From: from, Marker: own.Version, MarkerBy: own.WrittenBy, Taken: h.now().UTC()})
		if err == nil {
			return s, nil
		}
	}
	return layout.Snapshot{}, messages.WrapProductError(messages.ErrorPluginSnapshotFailed,
		fmt.Sprintf("could not snapshot the data of %s: %v", id, err), map[string]any{"id": id}, err)
}

// FinishUpgrade removes package id's upgrade marker once the new version
// passed its health gate.
func (h *Host) FinishUpgrade(id string) error {
	unlock, err := h.Dir.Lock()
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	return h.Dir.RemoveUpgrade(id)
}

// ApproveCurrent records current version's own approval hash in package
// id's control file when version is current and the file holds another
// hash, naming by as the approver: a rollback that was stopped after it
// made version current and before it wrote that approval is finished so.
// It changes nothing when another version is current.
func (h *Host) ApproveCurrent(id, version, by string) error {
	unlock, err := h.Dir.Lock()
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	if current, err := h.Dir.Current(id); err != nil || current != version {
		return err
	}
	v, err := h.recheckVersion(id, version, true)
	if err != nil {
		return err
	}
	hash, err := ApprovalHash(v)
	if err != nil {
		return err
	}
	ctl, _, err := h.Dir.ReadControl(id)
	if err != nil || ctl.ApprovedHash == string(hash) {
		return err
	}
	ctl.ApprovedHash, ctl.ApprovedBy, ctl.At = string(hash), by, h.now().UTC()
	return h.Dir.WriteControl(id, ctl)
}

// RollbackOptions are a rollback's choices.
type RollbackOptions struct {
	// To is the version to make current; empty is the newest unpacked
	// version older than the current one.
	To string
	// Restore puts the plugin's data back to a snapshot of To's data,
	// and the data marker to what it was then.
	Restore bool
	// Snapshot names the snapshot to restore; empty is the newest one
	// taken of To's data.
	Snapshot string
	// By names who rolled back, recorded as the approver of To.
	By string
	// Quiesce, when set, runs once every check passed and before anything
	// changes: the server stops the running version there.
	Quiesce func() error
}

// RolledBack is the outcome of a rollback.
type RolledBack struct {
	*Verified
	// From is the version that was current before.
	From string
	// Restored is the snapshot whose data was put back; empty without
	// RollbackOptions.Restore.
	Restored string
}

// Rollback makes an earlier unpacked version of installed package id
// current again, keeping whether it is enabled and recording To's own
// approval hash. The version must verify again, and its build may not be
// older than the data its plugin would find: the stored data marker, or
// the snapshot's when the data is restored (extapi.ErrorStateTooNew). With
// nothing to roll back to it is ErrorPluginNoPreviousVersion; a restore
// without a snapshot of To's data is ErrorPluginSnapshotMissing, and
// nothing changes. A restore writes the upgrade marker first (From is To),
// so a crash part-way is finished at the next boot; the data is put back
// before the data marker is lowered and only then is To made current.
// Any upgrade marker is gone afterwards. The plugin must not be running:
// Quiesce stops it.
func (h *Host) Rollback(id string, opt RollbackOptions) (*RolledBack, error) {
	if !extapi.ValidPackageID(id) {
		return nil, notInstalled(id)
	}
	unlock, err := h.Dir.Lock()
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock() }()
	current, err := h.Dir.Current(id)
	if err != nil {
		return nil, err
	}
	if current == "" {
		return nil, notInstalled(id)
	}
	versions, err := h.Dir.Versions(id)
	if err != nil {
		return nil, err
	}
	to := opt.To
	if to == "" {
		to = PreviousVersion(current, versions)
	}
	if to == "" || !slices.Contains(versions, to) || (to == current && !opt.Restore) {
		return nil, messages.NewProductError(messages.ErrorPluginNoPreviousVersion,
			fmt.Sprintf("%s has no previous version to roll back to", id), map[string]any{"id": id})
	}
	v, err := h.recheckVersion(id, to, true)
	if err != nil {
		return nil, err
	}
	out := &RolledBack{Verified: v, From: current}
	var snap layout.Snapshot
	if opt.Restore {
		if snap, err = h.snapshotOf(id, to, opt.Snapshot); err != nil {
			return nil, err
		}
		name := extapi.PluginDataMarker(id)
		if snap.Marker > 0 {
			if err := extapi.CheckMarkers([]extapi.Marker{{Name: name, Version: snap.Marker, WrittenBy: snap.MarkerBy}},
				map[string]int{name: stateVersion(v.Manifest)}); err != nil {
				return nil, err
			}
		}
	} else if err := h.CheckDataMarker(v.Manifest); err != nil {
		return nil, err
	}
	hash, err := ApprovalHash(v)
	if err != nil {
		return nil, err
	}
	ctl, _, err := h.Dir.ReadControl(id)
	if err != nil {
		return nil, err
	}
	if opt.Quiesce != nil {
		if err := opt.Quiesce(); err != nil {
			return nil, err
		}
	}
	if opt.Restore {
		if err := h.Dir.WriteUpgrade(id, layout.Upgrade{From: to, To: current, Snapshot: snap.Name, Started: h.now().UTC()}); err != nil {
			return nil, err
		}
		if err := h.Dir.Restore(id, snap.Name); err != nil {
			return nil, err
		}
		if err := h.restoreMarker(id, snap); err != nil {
			return nil, err
		}
		out.Restored = snap.Name
	}
	if _, err := h.Dir.Activate(id, to); err != nil {
		return nil, err
	}
	ctl.ApprovedHash, ctl.ApprovedBy, ctl.At = string(hash), opt.By, h.now().UTC()
	if err := h.Dir.WriteControl(id, ctl); err != nil {
		return nil, err
	}
	return out, h.Dir.RemoveUpgrade(id)
}

// snapshotOf is package id's snapshot called name, or its newest snapshot
// of version's data when name is empty.
func (h *Host) snapshotOf(id, version, name string) (layout.Snapshot, error) {
	missing := messages.NewProductError(messages.ErrorPluginSnapshotMissing,
		fmt.Sprintf("%s has no snapshot of its data from version %s", id, version), map[string]any{"id": id, "version": version})
	if name != "" {
		s, ok, err := h.Dir.FindSnapshot(id, name)
		switch {
		case err != nil:
			return layout.Snapshot{}, err
		case !ok || s.From != version:
			return layout.Snapshot{}, missing
		}
		return s, nil
	}
	list, err := h.Dir.ListSnapshots(id)
	if err != nil {
		return layout.Snapshot{}, err
	}
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].From == version {
			return list[i], nil
		}
	}
	return layout.Snapshot{}, missing
}

// restoreMarker puts package id's data marker back to what it was when
// snapshot s was taken: the one exception to a data marker never being
// lowered, since the data it describes is put back with it.
func (h *Host) restoreMarker(id string, s layout.Snapshot) error {
	name := extapi.PluginDataMarker(id)
	if s.Marker == 0 {
		return h.Markers.DeletePluginMarker(name)
	}
	return h.Markers.SetPluginMarker(extapi.Marker{Name: name, Version: s.Marker, WrittenBy: s.MarkerBy})
}

// PreviousVersion is the newest of versions older than current; empty
// when none is.
func PreviousVersion(current string, versions []string) string {
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

// checkNotUpgrading refuses to place a package over id while an upgrade
// of id is in progress (its marker exists): the upgrade owns the package
// until it passes its health gate or rolls back, also across a restart.
func (h *Host) checkNotUpgrading(id string) error {
	if _, ok, err := h.Dir.ReadUpgrade(id); err != nil || !ok {
		return err
	}
	return messages.NewProductError(messages.ErrorPluginUpgradeInProgress, fmt.Sprintf("%s is being upgraded", id), map[string]any{"id": id})
}
