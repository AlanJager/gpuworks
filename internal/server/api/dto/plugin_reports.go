package dto

import "time"

// PluginState is GET /api/v1/plugins/{id}/state: what the core's pushed
// state holds for one plugin, read from memory. Every instant is on the
// plugin clock (extclock).
type PluginState struct {
	ID string `json:"id"`
	// Installed is false for a plugin whose package is gone while its rows
	// are still held (an uninstall that failed half way); Verified is false
	// for one that no longer verifies, whose grants are dropped.
	Installed bool `json:"installed"`
	Verified  bool `json:"verified"`
	// Now is the plugin clock now.
	Now time.Time `json:"now"`
	// DownSince is when the plugin went down; absent while it is up.
	DownSince *time.Time `json:"down_since,omitempty"`
	// GraceS is its down grace in seconds; ClampAt is when its grants
	// clamp, absent while it is up.
	GraceS  int64      `json:"grace_s"`
	ClampAt *time.Time `json:"clamp_at,omitempty"`
	// Tables are the tables it pushed, by kind.
	Tables []PluginStateTable `json:"tables"`
	// Overrides are the administrators' overrides of its restrictions, by
	// id.
	Overrides []PluginOverride `json:"overrides"`
}

// PluginStateTable is one table a plugin pushed.
type PluginStateTable struct {
	Kind       string    `json:"kind"`
	Schema     int       `json:"schema"`
	Generation uint64    `json:"generation"`
	Digest     string    `json:"digest"`
	Entries    int       `json:"entries"`
	PushedAt   time.Time `json:"pushed_at"`
	// NextStep is when one of its timelines next takes a step; absent when
	// none is ahead.
	NextStep *time.Time `json:"next_step,omitempty"`
}

// PluginImpact is what uninstalling a plugin would change now (GET
// /api/v1/plugins/{id}/impact, and the impact of a DELETE refused with
// PLUGIN_UNINSTALL_CONFIRM_REQUIRED). DELETE ?confirm=<confirm> runs the
// uninstall while the impact is still this one.
type PluginImpact struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Confirm string `json:"confirm"`
	// Tables are the tables the uninstall deletes, by kind.
	Tables []PluginImpactTable `json:"tables"`
	// Nodes are the ids of the nodes whose hold it releases, ascending:
	// the nodes only it holds, and the nodes held for capacity when no cap
	// is left. A cap that stays but loosens frees nodes not counted here.
	Nodes []uint `json:"nodes"`
	// Features are the feature keys that become locked.
	Features []string `json:"features"`
	// Caps are the fleet caps lifted or loosened: max_nodes_with_cards,
	// max_cards.
	Caps []string `json:"caps"`
	// Banners counts the plugin's administrator banners shown now.
	Banners int `json:"banners"`
	// Overrides are the ids of its overrides, removed with it.
	Overrides []int64 `json:"overrides"`
	// UsersAffected counts the users whose roles or mappings it pushed.
	UsersAffected int `json:"users_affected"`
}

// PluginImpactTable is one table an uninstall deletes.
type PluginImpactTable struct {
	Kind    string `json:"kind"`
	Entries int    `json:"entries"`
}

// PluginCatalog is GET /api/v1/plugins/catalog: the packages the imported
// indexes offer joined with the installed ones, by id, each with the
// server's verdict.
type PluginCatalog struct {
	Items []PluginCatalogEntry `json:"items"`
	// Errors are the imported indexes that no longer verify and are left
	// out, by publisher.
	Errors []PluginCatalogError `json:"errors"`
}

// PluginCatalogEntry is one package of the catalog.
type PluginCatalogEntry struct {
	ID        string `json:"id"`
	Publisher string `json:"publisher,omitempty"`
	// Installed is the installed version; absent when it is not installed.
	Installed string `json:"installed,omitempty"`
	// BestVersion is the newest build this server can install; absent when
	// the indexes list none it can.
	BestVersion string `json:"best_version,omitempty"`
	MinCoreHint string `json:"min_core_hint,omitempty"`
	// Installable reports that BestVersion is not installed yet.
	Installable bool `json:"installable"`
	// Reason says why nothing newer is installable: the refusal of the
	// newest build refused; absent when none is.
	Reason *ProductMessage `json:"reason,omitempty"`
	// Refused are the builds newer than BestVersion this server refuses,
	// newest first.
	Refused []PluginCatalogRefusal `json:"refused"`
}

// PluginCatalogRefusal is a build the server will not install.
type PluginCatalogRefusal struct {
	Version     string         `json:"version"`
	MinCoreHint string         `json:"min_core_hint,omitempty"`
	Message     ProductMessage `json:"message"`
}

// PluginCatalogError is an imported index left out of the catalog.
type PluginCatalogError struct {
	Publisher string         `json:"publisher"`
	Message   ProductMessage `json:"message"`
}

// PluginEvent is the data of an SSE "state" frame of GET
// /api/v1/plugins/{id}/logs: one change of the plugin's supervision state.
type PluginEvent struct {
	// Seq numbers the plugin's events from 1 since the server started.
	Seq   uint64    `json:"seq"`
	At    time.Time `json:"at"`
	State string    `json:"state"`
	// Message says why it entered State; absent when nothing needs saying.
	Message *ProductMessage `json:"message,omitempty"`
	// Truncated reports that events before this one, after what the
	// stream had sent, were dropped (the server keeps the last 256).
	Truncated bool `json:"truncated,omitempty"`
}

// PluginStderr is the data of an SSE "stderr" frame of GET
// /api/v1/plugins/{id}/logs: output of the plugin's stderr.
type PluginStderr struct {
	// Offset is the byte offset of Text in everything the plugin wrote.
	Offset uint64 `json:"offset"`
	Text   string `json:"text"`
	// Truncated reports that output before Offset, after what the stream
	// had sent, was dropped (the server keeps the last 1 MiB).
	Truncated bool `json:"truncated,omitempty"`
}
