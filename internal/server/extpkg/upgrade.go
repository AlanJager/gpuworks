package extpkg

import (
	"fmt"
	"io"

	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// UpgradeOptions are an upgrade's choices.
type UpgradeOptions struct {
	// ApprovedBy names who approved the upgrade.
	ApprovedBy string
	// Approve must equal the incoming package's approval hash, unless the
	// hash is the approved one and the site policy auto-approves that.
	Approve extapi.Digest
	// Features, when set, refuses a package declaring a feature key
	// someone else provides (ErrorPluginFeatureTaken), before approval.
	Features FeatureOwners
	// ID, when set, is the package the upgrade is for: a package of
	// another id is refused (ErrorPluginUpgradeIDMismatch) before approval.
	ID string
	// Marker writes the upgrade marker (layout.Upgrade) once the package
	// is verified, approved and unpacked, before Quiesce; the caller
	// removes it once the new version is healthy (FinishUpgrade) or rolls
	// back. A refusal before the new version is current removes it again.
	Marker bool
	// Quiesce, when set, runs after the marker is written and before the
	// data is snapshot or the new version made current, with no lock of
	// the data directory held: the server stops the running version
	// there. When the upgrade fails before the new version is current,
	// the caller starts that version again.
	Quiesce func() error
	// Snapshot copies the plugin's data, with its data marker, before the
	// new version is made current (layout.TakeSnapshot), with no lock of
	// the data directory held; a copy that fails refuses the upgrade
	// (ErrorPluginSnapshotFailed).
	Snapshot bool
}

// Upgraded is the outcome of an upgrade.
type Upgraded struct {
	*Installed
	// Diff is what the new version asks for compared with the old one.
	Diff ApprovalDiff
	// AutoApproved reports that the approval hash was unchanged and the
	// site policy applied the upgrade without a new approval.
	AutoApproved bool
	// Snapshot is the copy of the data taken before the new version was
	// made current; zero without UpgradeOptions.Snapshot.
	Snapshot layout.Snapshot
	// From is the version that was current before.
	From string
	// Activated reports, on an upgrade that failed, that the new version
	// was made current before the failure: the upgrade marker stays and
	// the caller must roll back to From.
	Activated bool
}

// Upgrade replaces installed package id with the package read from r, the
// way Install places it, keeping whether the package is enabled. The
// package's id must be installed (ErrorPluginNotInstalled). Without
// approval it is refused with ErrorPluginApprovalRequired, whose params
// carry the diff from the installed version (an *ApprovalNeeded cause
// holds it too), and nothing changes; so is a version unpacked before
// with other contents (ErrorPluginVersionConflict). Only then are the
// marker written, the running version quiesced and the data snapshot,
// with no lock of the data directory held, and the new version made
// current under it. An approval hash equal to the approved one counts as
// approved when the site policy auto-approves unchanged upgrades. An
// error after the new version became current comes with the Upgraded,
// Activated set.
func (h *Host) Upgrade(r io.Reader, opt UpgradeOptions) (*Upgraded, error) {
	up := &Upgraded{}
	p, err := h.unpack(r, opt.Features, func(v *Verified, hash extapi.Digest) (layout.Control, error) {
		m := v.Manifest
		if opt.ID != "" && m.ID != opt.ID {
			return layout.Control{}, messages.NewProductError(messages.ErrorPluginUpgradeIDMismatch,
				fmt.Sprintf("the package is %s, not %s", m.ID, opt.ID), map[string]any{"id": opt.ID, "package_id": m.ID})
		}
		if err := h.checkNotUpgrading(m.ID); err != nil {
			return layout.Control{}, err
		}
		current, err := h.Dir.Current(m.ID)
		if err != nil {
			return layout.Control{}, err
		}
		if current == "" {
			return layout.Control{}, notInstalled(m.ID)
		}
		up.From = current
		ctl, _, err := h.Dir.ReadControl(m.ID)
		if err != nil {
			return layout.Control{}, err
		}
		up.Diff = h.diffFromCurrent(v, current)
		up.Diff.HashChanged = string(hash) != ctl.ApprovedHash
		up.AutoApproved = !up.Diff.HashChanged && h.Verifier.Policy.AutoApproveUnchanged
		approvedBy := opt.ApprovedBy
		switch {
		case up.AutoApproved:
			// Nothing new was approved: the earlier approver stands.
			approvedBy = ctl.ApprovedBy
		case opt.Approve != hash:
			return layout.Control{}, approvalRequired(m, hash, up.Diff)
		}
		return layout.Control{
			Enabled: ctl.Enabled, ApprovedHash: string(hash), ApprovedBy: approvedBy, At: h.now().UTC(),
			// Host setup steps already applied stay applied unless the
			// new version asks for one more.
			HostSetupApplied: ctl.HostSetupApplied && len(up.Diff.HostSetupAdded) == 0,
		}, nil
	})
	if err != nil {
		return nil, err
	}
	m := p.v.Manifest
	marked := false
	fail := func(err error) (*Upgraded, error) {
		if marked {
			_ = h.Dir.RemoveUpgrade(m.ID)
		}
		h.abandon(p)
		return nil, err
	}
	name := layout.SnapshotName(up.From, h.now())
	if opt.Marker {
		if err := h.Dir.WriteUpgrade(m.ID, layout.Upgrade{From: up.From, To: m.Version, Snapshot: name, Started: h.now().UTC()}); err != nil {
			return fail(err)
		}
		marked = true
	}
	if opt.Quiesce != nil {
		if err := opt.Quiesce(); err != nil {
			return fail(err)
		}
	}
	if opt.Snapshot {
		s, err := h.snapshot(m.ID, up.From, name)
		if err != nil {
			return fail(err)
		}
		up.Snapshot = s
	}
	in, activated, err := h.activate(p, func(*Verified, extapi.Digest) (layout.Control, error) {
		// The caller holds the package (the server by the lock of its
		// id): nothing else may have replaced the version it came from.
		current, err := h.Dir.Current(m.ID)
		if err != nil {
			return layout.Control{}, err
		}
		if current != up.From {
			return layout.Control{}, fmt.Errorf("%s changed from %s to %q during its upgrade", m.ID, up.From, current)
		}
		return p.ctl, nil
	})
	up.Installed = in
	if err != nil {
		if activated {
			up.Activated = true
			return up, err
		}
		return fail(err)
	}
	return up, nil
}
