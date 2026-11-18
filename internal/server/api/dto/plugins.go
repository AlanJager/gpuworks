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
	Isolation   string   `json:"isolation"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	// ApproveHash is the approval the administrator gave.
	ApproveHash string `json:"approve_hash,omitempty"`
	// Actions are what an administrator may do with it now: enable,
	// disable, uninstall.
	Actions []string `json:"actions"`
}

// PluginList is GET /api/v1/plugins.
type PluginList struct {
	Items []Plugin `json:"items"`
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
