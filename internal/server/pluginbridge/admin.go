package pluginbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// States a View reports beside the plugin host's (pluginhost.State):
// installed packages whose server part does not run, and why.
const (
	StateDisabled  = "disabled"                   // installed, not enabled
	StateInstalled = "installed"                  // enabled, with no server part to run
	StateIdle      = string(pluginhost.StateIdle) // enabled, an on-demand server part not running now
	StateBroken    = "broken"                     // installed, but no longer verifies; it does not run
	StateRevoked   = "revoked"                    // its build or its certificate is revoked; it does not run
	// StateNeedsHostSetup: enabled, but root has not applied its host
	// setup steps on this host (PLUGIN_HOST_SETUP_REQUIRED names the
	// command); it does not start until they are.
	StateNeedsHostSetup = "needs_host_setup"
)

// Actions a View offers its administrator.
const (
	ActionEnable    = "enable"
	ActionDisable   = "disable"
	ActionUninstall = "uninstall"
)

// View is one installed package as its administrator sees it.
type View struct {
	ID, Version, Publisher string
	// Trust is extpkg.TrustProject or extpkg.TrustHost; empty when the
	// package does not verify.
	Trust   string
	Enabled bool
	// State is a pluginhost.State for a supervised server part, or one of
	// the states above.
	State string
	// Code, Params and Message say why the package is in State; empty when
	// nothing needs saying.
	Code    string
	Params  map[string]any
	Message string
	// Since is when the server part entered State; zero when it is not
	// supervised.
	Since time.Time
	// Restarts counts the processes started after the first.
	Restarts int
	// Parts are the package's parts: server, node, ui, store.
	Parts []string
	// Isolation is how the server part runs (landlock or none).
	Isolation   string
	Roles       []string
	Permissions []string
	// ApproveHash is the approval the administrator gave.
	ApproveHash string
	// Actions are what the administrator may do with it now.
	Actions []string
}

// List is every installed package, sorted by id.
func (b *Bridge) List() []View {
	b.mu.Lock()
	entries := maps.Clone(b.entries)
	b.mu.Unlock()
	out := make([]View, 0, len(entries))
	for _, id := range slices.Sorted(maps.Keys(entries)) {
		out = append(out, b.viewOf(entries[id]))
	}
	return out
}

func (b *Bridge) viewOf(e extpkg.Entry) View {
	v := View{ID: e.ID, Version: e.Version, Enabled: e.Control.Enabled, ApproveHash: e.Control.ApprovedHash, Isolation: extpkg.HostIsolation()}
	switch {
	case e.Err != nil:
		v.State, v.Actions = StateBroken, []string{ActionUninstall}
		v.Code, v.Params, v.Message = codeOf(e.Err)
		return v
	case e.Verified == nil:
		v.State, v.Actions = StateBroken, []string{ActionUninstall}
		return v
	}
	ver := e.Verified
	m := ver.Manifest
	v.Publisher, v.Trust, v.Roles, v.Permissions, v.Parts = m.Publisher, ver.Trust, slices.Clone(m.Roles), slices.Clone(m.Permissions), partsOf(m)
	switch {
	case ver.Revoked != nil:
		v.State, v.Actions = StateRevoked, []string{ActionUninstall}
		v.Code, v.Params = messages.ErrorPluginPackageRevoked, map[string]any{"id": m.ID, "version": m.Version, "replacement": ver.Revoked.Replacement}
		v.Message = fmt.Sprintf("%s %s is revoked", m.ID, m.Version)
		return v
	case !e.Control.Enabled:
		v.State, v.Actions = StateDisabled, []string{ActionEnable, ActionUninstall}
		return v
	}
	v.Actions = []string{ActionDisable, ActionUninstall}
	switch {
	case m.Runtime == nil || ver.Binary == "":
		v.State = StateInstalled
		return v
	}
	b.mu.Lock()
	refused := b.refusals[m.ID]
	b.mu.Unlock()
	if refused != nil {
		v.State = string(pluginhost.StateFailed)
		v.Code, v.Params, v.Message = codeOf(refused)
		if v.Code == messages.ErrorPluginHostSetupRequired {
			// Enable checks the applied record again and starts the
			// plugin once root has run the command.
			v.State, v.Actions = StateNeedsHostSetup, []string{ActionEnable, ActionDisable, ActionUninstall}
		}
		return v
	}
	st, ok := b.host.Status(m.ID)
	if !ok {
		v.State = string(pluginhost.StateStarting)
		return v
	}
	v.State, v.Code, v.Params, v.Since, v.Restarts = string(st.State), st.Code, st.Params, st.Since, max(st.Starts-1, 0)
	if st.Isolation != "" {
		v.Isolation = st.Isolation
	}
	return v
}

func partsOf(m *extapi.Manifest) []string {
	var parts []string
	if m.Runtime != nil {
		parts = append(parts, "server")
	}
	if m.Node != nil {
		parts = append(parts, "node")
	}
	if m.UI != nil {
		parts = append(parts, "ui")
	}
	if m.Storage != nil {
		parts = append(parts, "store")
	}
	return parts
}

// codeOf is a refusal's code, params and fallback text.
func codeOf(err error) (string, map[string]any, string) {
	if err == nil {
		return "", nil, ""
	}
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		return pe.Code(), pe.Params(), pe.Error()
	}
	var xe *extapi.Error
	if errors.As(err, &xe) {
		return xe.Code, xe.Params, xe.Message
	}
	return "", nil, err.Error()
}

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
// refused (PLUGIN_FEATURE_TAKEN).
func (b *Bridge) Install(ctx context.Context, r io.Reader, req InstallRequest) (out *Installed, err error) {
	b.admin.Lock()
	defer b.admin.Unlock()
	id := ""
	defer func() { b.audit("install", id, req.By, err) }()
	in, err := b.pkgs.Install(r, extpkg.InstallOptions{Enable: req.Enable, ApprovedBy: req.By, RequireApproval: true, Approve: req.Approve, Features: b})
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
	b.admin.Lock()
	defer b.admin.Unlock()
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
	b.admin.Lock()
	defer b.admin.Unlock()
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

// Uninstall stops package id, removes it (with its data when purge is
// set) and removes everything it pushed, releasing its holds in one pass;
// running work is never stopped. Retrying finishes an uninstall that
// failed after the package was removed.
func (b *Bridge) Uninstall(ctx context.Context, id string, purge bool, by string) (err error) {
	b.admin.Lock()
	defer b.admin.Unlock()
	defer func() { b.audit("uninstall", id, by, err) }()
	if !extapi.ValidPackageID(id) {
		return notInstalled(id)
	}
	if err := b.stop(ctx, id); err != nil {
		return err
	}
	removeErr := b.pkgs.Remove(id, purge)
	if code, _, _ := codeOf(removeErr); removeErr != nil && code != messages.ErrorPluginNotInstalled {
		return removeErr
	}
	if err := b.refresh(); err != nil {
		return err
	}
	held := len(b.state.Held(id)) > 0
	if removeErr != nil && !held {
		return removeErr
	}
	return b.state.Uninstall(ctx, id)
}

func (b *Bridge) entry(id string) (extpkg.Entry, error) {
	b.mu.Lock()
	e, ok := b.entries[id]
	b.mu.Unlock()
	if !ok {
		return extpkg.Entry{}, notInstalled(id)
	}
	return e, nil
}

func (b *Bridge) view(id string) View {
	e, err := b.entry(id)
	if err != nil {
		return View{ID: id}
	}
	return b.viewOf(e)
}

func notInstalled(id string) error {
	return messages.NewProductError(messages.ErrorPluginNotInstalled, "no plugin "+id+" is installed", map[string]any{"id": id})
}

// audit records an administration call: who did what to which package,
// and how it ended.
func (b *Bridge) audit(action, id, by string, err error) {
	outcome, code := "ok", ""
	if err != nil {
		outcome = "refused"
		code, _, _ = codeOf(err)
	}
	b.log.Info("plugin administration", "audit", true, "action", action, "plugin", id, "by", by, "outcome", outcome, "code", code)
}
