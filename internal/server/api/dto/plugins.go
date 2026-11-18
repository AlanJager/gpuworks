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
	// Actions are what an administrator may do with it now: enable,
	// disable, uninstall.
	Actions []string `json:"actions"`
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
	// Check is the plugins diagnostics check over the installed packages;
	// absent with nothing installed, so that list stays the community
	// server's.
	Check *PluginsCheck `json:"check,omitempty"`
}

// PluginsCheck is the plugins diagnostics check: status fail while an
// enabled package is crashlooping, failed, broken or revoked, warn while
// one is incompatible with this server, pass otherwise; code and params
// (ids) say which.
type PluginsCheck struct {
	ID     string         `json:"id"`
	Status string         `json:"status"`
	Code   string         `json:"code"`
	Params map[string]any `json:"params"`
	// Message is the English fallback for Code.
	Message string `json:"message,omitempty"`
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
