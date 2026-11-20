package app

// HelloInventory is what the node holds that the connection handshake reports
// (conn.HelloInventory): the engine's validated content-addressed asset cache
// and the managed directory groups (proto.Hello.ManagedDirGroups), without
// coupling conn to engine internals. managedDirs is false only without an
// engine, which can hold no managed directory.
func (n *NodeRuntime) HelloInventory() (assetDigests, managedDirGroups []string, managedDirs bool) {
	if n.engine == nil {
		return nil, nil, false
	}
	return n.engine.CachedRuntimeAssetDigests(), n.engine.ManagedDirGroups(), true
}
