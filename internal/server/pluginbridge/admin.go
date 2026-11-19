package pluginbridge

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
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

// Actions a View offers its administrator. Purge is the uninstall that
// also removes the package's data (DELETE ?purge=true).
const (
	ActionEnable    = "enable"
	ActionDisable   = "disable"
	ActionRestart   = "restart"
	ActionUninstall = "uninstall"
	ActionPurge     = "purge"
	// ActionUpgrade (POST /plugins/{id}/upgrade) is offered for every
	// installed package, ActionRollback (POST /plugins/{id}/rollback) for
	// one with a previous version.
	ActionUpgrade  = "upgrade"
	ActionRollback = "rollback"
)

// removal is what every installed package offers: uninstall, with or
// without its data.
var removal = []string{ActionUninstall, ActionPurge}

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
	// Previous is the newest unpacked version older than Version, kept for
	// rollback; empty when there is none.
	Previous string
	// OnDemand marks a server part that starts on its first call.
	OnDemand bool
	// Pending is the newer build the imported indexes offer for it (its
	// version); empty when none does.
	Pending string
	// Features are the feature keys it declares with the access each has
	// now, by key.
	Features []FeatureView
	// Actions are what the administrator may do with it now.
	Actions []string
}

// FeatureView is one declared feature key and its access now (full,
// read_only or locked).
type FeatureView struct {
	Key, Access string
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
	v := b.stateOf(e)
	v.Actions = append(v.Actions, ActionUpgrade)
	if v.Previous != "" {
		v.Actions = append(v.Actions, ActionRollback)
	}
	return v
}

// stateOf is viewOf without the upgrade actions every state offers.
func (b *Bridge) stateOf(e extpkg.Entry) View {
	v := View{ID: e.ID, Version: e.Version, Enabled: e.Control.Enabled, ApproveHash: e.Control.ApprovedHash, Isolation: extpkg.HostIsolation(),
		Previous: previousOf(e.Version, e.Versions)}
	b.mu.Lock()
	v.Pending = b.pending[e.ID]
	b.mu.Unlock()
	switch {
	case e.Err != nil:
		v.State, v.Actions = StateBroken, actions(removal)
		v.Code, v.Params, v.Message = codeOf(e.Err)
		return v
	case e.Verified == nil:
		v.State, v.Actions = StateBroken, actions(removal)
		return v
	}
	ver := e.Verified
	m := ver.Manifest
	v.Publisher, v.Trust, v.Roles, v.Permissions, v.Parts = m.Publisher, ver.Trust, slices.Clone(m.Roles), slices.Clone(m.Permissions), partsOf(m)
	v.OnDemand = m.Runtime != nil && m.Runtime.Lifecycle == extapi.LifecycleOnDemand
	for _, f := range m.Features {
		v.Features = append(v.Features, FeatureView{Key: f.Key, Access: b.state.Availability(featuregate.Key(f.Key)).Access.String()})
	}
	slices.SortFunc(v.Features, func(a, b FeatureView) int { return strings.Compare(a.Key, b.Key) })
	switch {
	case ver.Revoked != nil:
		v.State, v.Actions = StateRevoked, actions(removal)
		v.Code, v.Params = messages.ErrorPluginPackageRevoked, map[string]any{"id": m.ID, "version": m.Version, "replacement": ver.Revoked.Replacement}
		v.Message = fmt.Sprintf("%s %s is revoked", m.ID, m.Version)
		return v
	case !e.Control.Enabled:
		v.State, v.Actions = StateDisabled, actions([]string{ActionEnable}, removal)
		return v
	}
	switch {
	case m.Runtime == nil || ver.Binary == "":
		v.State, v.Actions = StateInstalled, actions([]string{ActionDisable}, removal)
		return v
	}
	v.Actions = actions([]string{ActionDisable, ActionRestart}, removal)
	b.mu.Lock()
	refused := b.refusals[m.ID]
	b.mu.Unlock()
	if refused != nil {
		v.State = string(pluginhost.StateFailed)
		v.Code, v.Params, v.Message = codeOf(refused)
		if v.Code == messages.ErrorPluginHostSetupRequired {
			// Enable checks the applied record again and starts the
			// plugin once root has run the command.
			v.State, v.Actions = StateNeedsHostSetup, actions([]string{ActionEnable, ActionDisable}, removal)
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

// actions joins lists of actions into a list of its own.
func actions(lists ...[]string) []string { return slices.Concat(lists...) }

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
