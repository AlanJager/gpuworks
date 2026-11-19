package dto

import "time"

// Plugin is one installed plugin package (GET /api/v1/plugins).
type Plugin struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Publisher string `json:"publisher,omitempty"`
	// Trust is project (certified by a project root) or host (by an issuer
	// server.yaml trusts); empty when the package no longer verifies.
	Trust   string `json:"trust,omitempty"`
	Enabled bool   `json:"enabled"`
	// State is where the package stands: disabled, installed, idle,
	// broken or revoked when its server part does not run; otherwise its
	// process state (starting, running, degraded, backoff, crashloop,
	// stopping, stopped, failed).
	State string `json:"state"`
	// Message says why the package is in State; absent when nothing needs
	// saying.
	Message *ProductMessage `json:"message,omitempty"`
	// Since is when the server part entered State; absent when it is not
	// supervised.
	Since    *time.Time `json:"since,omitempty"`
	Restarts int        `json:"restarts"`
	// Parts are the package's parts: server, node, ui, store.
	Parts []string `json:"parts"`
	// Isolation is how the server part runs: landlock or none.
	Isolation string   `json:"isolation"`
	Roles     []string `json:"roles"`
	// Permissions are the approved permissions, sorted by name, each with
	// the risk class the server gives it.
	Permissions []PluginPermission `json:"permissions"`
	// ApproveHash is the approval the administrator gave.
	ApproveHash string `json:"approve_hash,omitempty"`
	// Previous is the newest unpacked version older than Version, kept for
	// rollback; absent when there is none.
	Previous string `json:"previous,omitempty"`
	// OnDemand marks a server part that starts on its first call and goes
	// idle without calls.
	OnDemand bool `json:"on_demand"`
	// Pending is the newer build the imported package indexes offer;
	// absent when they offer none.
	Pending *PluginPending `json:"pending,omitempty"`
	// Features are the feature keys the package declares, by key, each with
	// its availability now: full, read_only or locked.
	Features []PluginFeature `json:"features"`
	// Actions are what an administrator may do with it now, computed by the
	// server: enable, disable, restart, uninstall, purge (uninstall with
	// ?purge=true), upgrade, rollback (with a previous version).
	Actions []string `json:"actions"`
}

// PluginPending is a newer build of an installed package.
type PluginPending struct {
	Version string `json:"version"`
}

// PluginFeature is one feature key a package declares.
type PluginFeature struct {
	Key string `json:"key"`
	// Availability is full, read_only or locked.
	Availability string `json:"availability"`
}

// PluginPermission is one permission of a plugin package.
type PluginPermission struct {
	Name string `json:"name"`
	// Risk is low, elevated or high.
	Risk string `json:"risk"`
}

// PluginList is GET /api/v1/plugins.
type PluginList struct {
	Items []Plugin `json:"items"`
	// Check is the plugins diagnostics check over the installed packages:
	// status fail while an enabled package is crashlooping, failed, broken
	// or revoked, warn while one is incompatible with this server, pass
	// otherwise; the message's code and params (ids) say which. Absent with
	// nothing installed, so the list is what it was before plugins.
	Check *DiagnosticCheck `json:"check,omitempty"`
}

// PluginInstalled answers POST /api/v1/plugins/install.
type PluginInstalled struct {
	Plugin Plugin `json:"plugin"`
	// Previous is the version that was current before; absent for a first
	// install.
	Previous string `json:"previous,omitempty"`
	// Unchanged reports that this exact build was already installed.
	Unchanged bool `json:"unchanged"`
}

// PluginUpgraded answers POST /api/v1/plugins/{id}/upgrade once the new
// version passed its health gate.
type PluginUpgraded struct {
	Plugin Plugin `json:"plugin"`
	// Previous is the version that was current before, kept for rollback
	// with a snapshot of its data.
	Previous string `json:"previous"`
	// AutoApproved reports that the approval hash did not change and the
	// site policy applied the upgrade without a new approval.
	AutoApproved bool `json:"auto_approved"`
	// Unchanged reports that this exact build was already unpacked.
	Unchanged bool `json:"unchanged"`
}

// PluginOverride is an administrator's release of one restriction entry a
// plugin pushed (GET and POST /api/v1/plugins/overrides). The plugin's next
// push that changes the entry supersedes it.
type PluginOverride struct {
	ID int64 `json:"id"`
	// Owner is the plugin whose restriction is released.
	Owner string `json:"owner"`
	// Kind is node_holds or fleet_capacity.
	Kind string `json:"kind"`
	// Entry is a node id in decimal for node_holds, max_nodes_with_cards
	// or max_cards for fleet_capacity.
	Entry string `json:"entry"`
	// Generation is the owner's generation of the kind when the override
	// was made.
	Generation uint64 `json:"generation"`
	// Action is delete or loosen.
	Action string `json:"action"`
	// Value is the raised cap of a loosen; absent for a delete.
	Value *int64 `json:"value,omitempty"`
	// ByUser is the id of the administrator who made it.
	ByUser uint      `json:"by_user"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// PluginOverrideList is GET /api/v1/plugins/overrides.
type PluginOverrideList struct {
	Items []PluginOverride `json:"items"`
}

// PluginOverrideRequest is POST /api/v1/plugins/overrides.
type PluginOverrideRequest struct {
	Owner  string `json:"owner"`
	Kind   string `json:"kind"`
	Entry  string `json:"entry"`
	Action string `json:"action"`
	// Value is the raised cap of a loosen; a delete takes none.
	Value  *int64 `json:"value,omitempty"`
	Reason string `json:"reason"`
}
