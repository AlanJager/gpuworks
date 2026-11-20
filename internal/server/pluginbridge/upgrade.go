package pluginbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// DefaultUpgradeGate is how long a new version must stay up after an
// upgrade before the upgrade counts as done.
const DefaultUpgradeGate = 60 * time.Second

// gateStartSlack is how much longer than the gate the new version may take
// to come up at all (its handshake, a slow first start).
const gateStartSlack = 20 * time.Second

// UpgradeRequest is one upgrade through the API.
type UpgradeRequest struct {
	// Approve must be the new version's approval hash, unless it is the
	// approved one and the site auto-approves unchanged upgrades; without
	// it the upgrade is refused with PLUGIN_APPROVAL_REQUIRED, whose params
	// carry the diff from the installed version, and nothing changes.
	Approve extapi.Digest
	// By names the administrator, for the audit record.
	By string
}

// Upgraded is the outcome of an upgrade that passed its health gate.
type Upgraded struct {
	View View
	// Previous is the version that was current before.
	Previous string
	// Diff is what the new version asks for compared with Previous.
	Diff extpkg.ApprovalDiff
	// AutoApproved reports that the site policy applied an upgrade whose
	// approval hash did not change.
	AutoApproved bool
	// Unchanged reports that this exact build was already unpacked.
	Unchanged bool
}

// Upgrade replaces installed package id with the package read from r
// (docs/dev/extensions.md): verified and approved first (a package of
// another id is PLUGIN_UPGRADE_ID_MISMATCH), then the upgrade marker is
// written, the running version stopped, its data copied (a copy that fails
// is PLUGIN_SNAPSHOT_FAILED and the old version starts again), the new
// version made current and started, and it must come up and stay up for
// the health gate. If it does not, it is stopped, the old version and its
// data are restored from the snapshot and started, and the upgrade is
// refused with PLUGIN_UPGRADE_ROLLED_BACK. What the package pushed stays
// enforced throughout; the package counts as down from the stop until a
// version is up again, and the restarts do not renew its grace. A disabled
// package, or one without a server part, has nothing to gate. The upgrade
// runs to its end even when the request is cancelled; it holds only the
// lock of id, and a disable or uninstall of id waiting more than 10 s ends
// its gate.
func (b *Bridge) Upgrade(ctx context.Context, id string, r io.Reader, req UpgradeRequest) (out *Upgraded, err error) {
	ctx = context.WithoutCancel(ctx)
	b.mu.Lock()
	if b.stopping {
		b.mu.Unlock()
		return nil, messages.NewProductError(messages.ErrorPluginUnavailable, "the server is stopping", map[string]any{"id": id})
	}
	// Under mu, so that no upgrade is added once Run waits for them.
	b.upgrades.Add(1)
	b.mu.Unlock()
	defer b.upgrades.Done()
	defer b.locks.lock(id)()
	defer func() { b.audit("upgrade", id, req.By, err) }()
	if _, err := b.entry(id); err != nil {
		return nil, err
	}
	quiesced := false
	up, err := b.pkgs.Upgrade(r, extpkg.UpgradeOptions{ID: id, ApprovedBy: req.By, Approve: req.Approve, Features: b,
		Marker: true, Snapshot: true,
		Quiesce: func() error {
			quiesced = true
			stopCtx, cancel := context.WithTimeout(ctx, stopWait)
			defer cancel()
			return b.stop(stopCtx, id)
		}})
	if err != nil {
		if up != nil && up.Activated {
			// The new version became current before the failure: the
			// marker stays, and the upgrade is undone like a failed gate.
			b.log.Error("plugin upgrade failed after the new version became current", "plugin", id, "err", err)
			return nil, b.rollBack(ctx, id, up, causeOf(err))
		}
		if quiesced {
			// The version that ran is still current: it runs again.
			b.startIfEnabled(id)
		}
		return nil, err
	}
	out = &Upgraded{Previous: up.From, Diff: up.Diff, AutoApproved: up.AutoApproved, Unchanged: up.Unchanged}
	if err := b.refresh(); err != nil {
		b.log.Error("plugin packages not read again after an upgrade", "plugin", id, "err", err)
		return nil, b.rollBack(ctx, id, up, causeOf(err))
	}
	if cause := b.gate(ctx, id); cause != "" {
		return nil, b.rollBack(ctx, id, up, cause)
	}
	if err := b.pkgs.FinishUpgrade(id); err != nil {
		b.log.Warn("plugin upgrade marker not removed; the next boot rolls the upgrade back", "plugin", id, "err", err)
	}
	out.View = b.view(id)
	return out, nil
}

// gate starts the new version of package id and waits out its health
// gate. It returns the code of why it failed (PLUGIN_UPGRADE_CANCELLED
// when a disable, an uninstall or the server's stop ended it), empty once
// the version passed or has nothing to gate: disabled, without a server
// part, or waiting for root's host setup, which an administrator approved
// with the upgrade.
func (b *Bridge) gate(ctx context.Context, id string) string {
	e, err := b.entry(id)
	if err != nil {
		return messages.ErrorPluginNotInstalled
	}
	if e.Err != nil {
		code, _, _ := codeOf(e.Err)
		return orCode(code, messages.ErrorPluginBroken)
	}
	if !runnable(e) {
		return ""
	}
	b.startIfEnabled(id)
	b.mu.Lock()
	refused := b.refusals[id]
	b.mu.Unlock()
	if refused != nil {
		code, _, _ := codeOf(refused)
		if code == messages.ErrorPluginHostSetupRequired {
			return ""
		}
		return orCode(code, messages.ErrorPluginUnavailable)
	}
	gctx, cancel := context.WithTimeout(ctx, b.upgradeGate+gateStartSlack)
	defer cancel()
	defer b.locks.gate(id, cancel)()
	// A server that began stopping before the gate was registered did not
	// cancel it.
	b.mu.Lock()
	if b.stopping {
		cancel()
	}
	b.mu.Unlock()
	err = b.host.AwaitHealthy(gctx, id, b.upgradeGate)
	if err == nil {
		return ""
	}
	if errors.Is(gctx.Err(), context.Canceled) {
		return messages.ErrorPluginUpgradeCancelled
	}
	var ue *pluginhost.UnhealthyError
	if errors.As(err, &ue) {
		return ue.Code()
	}
	return messages.ErrorPluginUnavailable
}

// rollBack undoes upgrade up of package id after it failed with cause:
// the new version is stopped, the old one made current with its data from
// the snapshot, and started. It returns PLUGIN_UPGRADE_ROLLED_BACK; when
// the rollback itself fails, that error (the upgrade marker stays, so the
// next boot tries again).
func (b *Bridge) rollBack(ctx context.Context, id string, up *extpkg.Upgraded, cause string) error {
	from, to := up.From, up.Manifest.Version
	stopCtx, cancel := context.WithTimeout(ctx, stopWait)
	err := b.stop(stopCtx, id)
	cancel()
	if err == nil {
		_, err = b.pkgs.Rollback(id, extpkg.RollbackOptions{To: from, Restore: true, Snapshot: up.Snapshot.Name, By: "automatic rollback"})
	}
	if err != nil {
		b.log.Error("plugin upgrade failed and could not be rolled back; the next boot tries again", "audit", true, "plugin", id,
			"from", from, "to", to, "cause_code", cause, "err", err)
		_ = b.refresh()
		return fmt.Errorf("upgrade of %s to %s failed (%s) and could not be rolled back: %w", id, to, cause, err)
	}
	b.log.Warn("plugin upgrade rolled back", "audit", true, "plugin", id, "from", from, "to", to, "cause_code", cause, "snapshot", up.Snapshot.Name)
	if err := b.refresh(); err != nil {
		b.log.Error("plugin packages not read again after a rollback", "plugin", id, "err", err)
	}
	b.startIfEnabled(id)
	return messages.NewProductError(messages.ErrorPluginUpgradeRolledBack,
		fmt.Sprintf("upgrade of %s to %s failed (%s); %s was restored", id, to, cause, from),
		map[string]any{"id": id, "version": to, "previous": from, "cause_code": cause})
}

// RollbackRequest is one rollback through the API.
type RollbackRequest struct {
	// RestoreState puts the plugin's data back to the newest snapshot of
	// the previous version's data, and its data marker with it.
	RestoreState bool
	// By names the administrator, for the audit record.
	By string
}

// Rollback makes package id's previous version (View.Previous) current
// again and starts it when the package is enabled. Without RestoreState
// the data stays as the newer version left it, and a previous version
// older than that data is refused (PLUGIN_STATE_TOO_NEW); with it the
// data comes back from the snapshot taken when that version was upgraded
// (PLUGIN_SNAPSHOT_MISSING when none is kept). No previous version is
// PLUGIN_NO_PREVIOUS_VERSION. A refused rollback leaves the running
// version running.
func (b *Bridge) Rollback(ctx context.Context, id string, req RollbackRequest) (v View, err error) {
	ctx = context.WithoutCancel(ctx)
	defer b.locks.lock(id)()
	defer func() { b.audit("rollback", id, req.By, err) }()
	if _, err := b.entry(id); err != nil {
		return View{}, err
	}
	quiesced := false
	_, err = b.pkgs.Rollback(id, extpkg.RollbackOptions{Restore: req.RestoreState, By: req.By, Quiesce: func() error {
		quiesced = true
		stopCtx, cancel := context.WithTimeout(ctx, stopWait)
		defer cancel()
		return b.stop(stopCtx, id)
	}})
	if err != nil {
		if quiesced {
			b.startIfEnabled(id)
		}
		return View{}, err
	}
	if err := b.refresh(); err != nil {
		return View{}, err
	}
	b.startIfEnabled(id)
	return b.view(id), nil
}

// causeOf is the code of err as a cause_code, PLUGIN_UNAVAILABLE when it
// carries none.
func causeOf(err error) string {
	code, _, _ := codeOf(err)
	return orCode(code, messages.ErrorPluginUnavailable)
}

func orCode(code, fallback string) string {
	if code == "" {
		return fallback
	}
	return code
}
