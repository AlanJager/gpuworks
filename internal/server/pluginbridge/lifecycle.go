package pluginbridge

import (
	"context"
	"io"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// InstallRequest is one install through the API.
type InstallRequest struct {
	// Approve must be the package's approval hash; without it the install
	// is refused with PLUGIN_APPROVAL_REQUIRED, which names it.
	Approve extapi.Digest
	// Enable starts the package's server part once installed. A package
	// installed over an enabled version stays enabled either way.
	Enable bool
	// By names the administrator, for the audit record.
	By string
}

// Installed is the outcome of an install.
type Installed struct {
	View View
	// Previous is the version that was current before; empty for a first
	// install.
	Previous string
	// Unchanged reports that this exact build was already installed.
	Unchanged bool
}

// Install installs the package read from r, as approved, and starts its
// server part when it is enabled and Run is running; a version it replaces
// is stopped first. The feature keys it declares are declared at once,
// and what it pushes is enforced as soon as it pushes. A package declaring
// a key a compiled module or another installed package provides is
// refused (PLUGIN_FEATURE_TAKEN), and one whose id names a compiled module
// (PLUGIN_ID_TAKEN).
func (b *Bridge) Install(ctx context.Context, r io.Reader, req InstallRequest) (out *Installed, err error) {
	id := ""
	unlock := func() {}
	defer func() { unlock() }()
	defer func() { b.audit("install", id, req.By, err) }()
	// The package is unpacked under the plugins directory's lock (refused
	// while an upgrade of it is under way), and made current and its
	// process changed under the lock of its id.
	in, err := b.pkgs.Install(r, extpkg.InstallOptions{Enable: req.Enable, ApprovedBy: req.By, RequireApproval: true, Approve: req.Approve, Features: b,
		ReservedIDs: b.cfg.CompiledModules, Claim: func(pid string) { id, unlock = pid, b.locks.lock(pid) }})
	if err != nil {
		if code, params, _ := codeOf(err); code == messages.ErrorPluginApprovalRequired {
			id, _ = params["id"].(string)
		}
		return nil, err
	}
	id = in.Manifest.ID
	st, supervised := b.host.Status(id)
	// The same build installed again over its running process keeps it.
	keep := in.Unchanged && in.Enabled && supervised && st.State != pluginhost.StateStopped && st.State != pluginhost.StateFailed
	if !keep {
		if err := b.stop(ctx, id); err != nil {
			return nil, err
		}
	}
	if err := b.refresh(); err != nil {
		return nil, err
	}
	if !keep {
		b.startIfEnabled(id)
	}
	return &Installed{View: b.view(id), Previous: in.Previous, Unchanged: in.Unchanged}, nil
}

// Enable lets package id run and starts its server part. A package that
// no longer verifies is refused (PLUGIN_BROKEN).
func (b *Bridge) Enable(ctx context.Context, id, by string) (v View, err error) {
	defer b.locks.lock(id)()
	defer func() { b.audit("enable", id, by, err) }()
	e, err := b.entry(id)
	if err != nil {
		return View{}, err
	}
	if e.Err != nil || e.Verified == nil || e.Verified.Revoked != nil {
		return View{}, messages.WrapProductError(messages.ErrorPluginBroken, "plugin "+id+" no longer verifies", map[string]any{"id": id}, e.Err)
	}
	if err := b.pkgs.SetEnabled(id, true); err != nil {
		return View{}, err
	}
	if err := b.refresh(); err != nil {
		return View{}, err
	}
	if st, ok := b.host.Status(id); !ok || st.State == pluginhost.StateStopped || st.State == pluginhost.StateFailed {
		b.startIfEnabled(id)
	}
	return b.view(id), nil
}

// Disable stops package id's server part and keeps it from starting.
// What it pushed stays enforced, and it counts as down, so its grants
// narrow once its grace has passed.
func (b *Bridge) Disable(ctx context.Context, id, by string) (v View, err error) {
	// An upgrade of id in its health gate is rolled back after 10 s.
	defer b.locks.lockCancelling(id, b.cancelWait)()
	defer func() { b.audit("disable", id, by, err) }()
	if _, err := b.entry(id); err != nil {
		return View{}, err
	}
	if err := b.pkgs.SetEnabled(id, false); err != nil {
		return View{}, err
	}
	if err := b.stop(ctx, id); err != nil {
		return View{}, err
	}
	if err := b.refresh(); err != nil {
		return View{}, err
	}
	return b.view(id), nil
}

// UninstallRequest is one uninstall through the API.
type UninstallRequest struct {
	// Purge removes the package's data, its snapshots and its data marker
	// too.
	Purge bool
	// Confirm must be the digest of the impact the administrator saw
	// (Impact); until it is, the uninstall is refused with
	// PLUGIN_UNINSTALL_CONFIRM_REQUIRED and the impact it would have.
	Confirm extapi.Digest
	// By names the administrator, for the audit record.
	By string
}

// Uninstall stops package id, removes it (with its data when purge is
// set) and removes everything it pushed, releasing its holds in one pass;
// running work is never stopped. It runs only once req.Confirm is the
// digest of what it changes now (Impact); otherwise it changes nothing and
// returns that impact with PLUGIN_UNINSTALL_CONFIRM_REQUIRED, whose params
// count it. Retrying finishes an uninstall that failed after the package
// was removed: its impact is then that of the rows left.
func (b *Bridge) Uninstall(ctx context.Context, id string, req UninstallRequest) (im pushstate.Impact, err error) {
	// An upgrade of id in its health gate is rolled back after 10 s.
	defer b.locks.lockCancelling(id, b.cancelWait)()
	defer func() { b.audit("uninstall", id, req.By, err) }()
	im, err = b.impactLocked(id)
	if err != nil {
		return pushstate.Impact{}, err
	}
	if req.Confirm != im.Digest {
		return im, confirmRequired(im, req.Confirm)
	}
	if err := b.stop(ctx, id); err != nil {
		return im, err
	}
	removeErr := b.pkgs.Remove(id, req.Purge)
	if code, _, _ := codeOf(removeErr); removeErr != nil && code != messages.ErrorPluginNotInstalled {
		return im, removeErr
	}
	if err := b.refresh(); err != nil {
		return im, err
	}
	b.forgetHealth(id)
	held := len(b.state.Held(id)) > 0
	if removeErr != nil && !held {
		return im, removeErr
	}
	return im, b.state.Uninstall(ctx, id)
}

// Restart stops package id's server part and starts it again as a new
// supervised instance, so its crash history and backoff start afresh; an
// on-demand one goes back to idle until its next call. A disabled package
// is refused (PLUGIN_DISABLED), one that no longer verifies or is revoked
// too (PLUGIN_BROKEN: Recheck happens on enable), and one without a server
// part has nothing to restart (PLUGIN_UNAVAILABLE).
func (b *Bridge) Restart(ctx context.Context, id, by string) (v View, err error) {
	defer b.locks.lock(id)()
	defer func() { b.audit("restart", id, by, err) }()
	e, err := b.entry(id)
	if err != nil {
		return View{}, err
	}
	switch {
	case e.Err != nil || e.Verified == nil || e.Verified.Revoked != nil:
		return View{}, messages.WrapProductError(messages.ErrorPluginBroken, "plugin "+id+" no longer verifies", map[string]any{"id": id}, e.Err)
	case !e.Control.Enabled:
		return View{}, messages.NewProductError(messages.ErrorPluginDisabled, "plugin "+id+" is disabled", map[string]any{"id": id})
	case !runnable(e):
		return View{}, pluginhost.Unavailable(id, StateInstalled, nil)
	}
	if err := b.stop(ctx, id); err != nil {
		return View{}, err
	}
	b.startIfEnabled(id)
	return b.view(id), nil
}
