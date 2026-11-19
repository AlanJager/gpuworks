package pluginbridge

// Where a per-package artifact lives.
const (
	ArtifactDB     = "db"
	ArtifactFile   = "file"
	ArtifactMemory = "memory"
)

// Artifact is one kind of state the server keeps per installed package
// (plugin-framework §6.2, docs/dev/extensions.md). An uninstall removes
// every artifact of the package; one marked Purge only when the uninstall
// purges, so a reinstall cannot load data an older build wrote.
type Artifact struct {
	// Name is "<where>:<what>": the table, the path under the package's
	// directory, or the structure holding it in memory.
	Name  string
	Where string
	Purge bool
}

// PerPackageArtifacts lists every kind of state kept per package. A new
// table, file or in-memory structure keyed by a package id joins this list
// in the change that adds it, and the uninstall that removes it: the unit
// test of this package fails on a pstate_* table or a map of the bridge,
// the plugin host or pushed state that it does not classify, and the
// contract test TestUninstallLeavesNothingUnderThePackageID fails on an
// entry it has no check for.
var PerPackageArtifacts = []Artifact{
	// What the package pushed, its down record and the overrides of it:
	// one commit of pushed state's writer removes them all.
	{Name: "db:pstate_owner", Where: ArtifactDB},
	{Name: "db:pstate_availability", Where: ArtifactDB},
	{Name: "db:pstate_availability_row", Where: ArtifactDB},
	{Name: "db:pstate_availability_step", Where: ArtifactDB},
	{Name: "db:pstate_availability_limit", Where: ArtifactDB},
	{Name: "db:pstate_fleet_capacity", Where: ArtifactDB},
	{Name: "db:pstate_fleet_capacity_step", Where: ArtifactDB},
	{Name: "db:pstate_fleet_capacity_keep", Where: ArtifactDB},
	{Name: "db:pstate_node_hold", Where: ArtifactDB},
	{Name: "db:pstate_banner", Where: ArtifactDB},
	{Name: "db:pstate_banner_step", Where: ArtifactDB},
	{Name: "db:pstate_attestation", Where: ArtifactDB},
	{Name: "db:pstate_notice_param", Where: ArtifactDB},
	{Name: "db:pstate_down", Where: ArtifactDB},
	{Name: "db:pstate_override", Where: ArtifactDB},
	// The data marker plugin.<id>.data (Q14: purge only).
	{Name: "db:plugin_markers", Where: ArtifactDB, Purge: true},

	// Under <data>/plugins/<id>/: the unpacked versions and the control
	// file go with the uninstall; data, tmp and snapshots with a purge,
	// which moves the whole directory to .trash/.
	{Name: "file:pkg", Where: ArtifactFile},
	{Name: "file:control.json", Where: ArtifactFile},
	{Name: "file:data", Where: ArtifactFile, Purge: true},
	{Name: "file:tmp", Where: ArtifactFile, Purge: true},
	{Name: "file:snapshots", Where: ArtifactFile, Purge: true},

	// In memory: the bridge's entries and what it binds to a started
	// package, the route table compiled from them, the health banners and
	// the dynamic feature set; the plugin host's instance, counters and
	// logs (with their event ring); pushed state's parts, liveness and
	// overrides.
	{Name: "memory:bridge.entries", Where: ArtifactMemory},
	{Name: "memory:bridge.pushers", Where: ArtifactMemory},
	{Name: "memory:bridge.refusals", Where: ArtifactMemory},
	{Name: "memory:bridge.crashloops", Where: ArtifactMemory},
	{Name: "memory:bridge.known", Where: ArtifactMemory},
	{Name: "memory:bridge.pending", Where: ArtifactMemory},
	{Name: "memory:bridge.routes", Where: ArtifactMemory},
	{Name: "memory:bridge.banners", Where: ArtifactMemory},
	{Name: "memory:features", Where: ArtifactMemory},
	{Name: "memory:host.plugins", Where: ArtifactMemory},
	{Name: "memory:host.metrics", Where: ArtifactMemory},
	{Name: "memory:host.logs", Where: ArtifactMemory},
	{Name: "memory:state.parts", Where: ArtifactMemory},
	{Name: "memory:state.spawn", Where: ArtifactMemory},
	{Name: "memory:state.ready", Where: ArtifactMemory},
	{Name: "memory:state.down", Where: ArtifactMemory},
	{Name: "memory:state.downDirty", Where: ArtifactMemory},
	{Name: "memory:state.overrides", Where: ArtifactMemory},
	{Name: "memory:state.superseded", Where: ArtifactMemory},
	{Name: "memory:state.reg", Where: ArtifactMemory},
}
