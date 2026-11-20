package conn

// HelloInventory is a handler's report of what the node holds, for the hello:
// its validated asset cache and its managed directory groups. A handler that
// can hold managed directories says so (managedDirs), and the hello then
// advertises proto.CapManagedDirs.
type HelloInventory interface {
	HelloInventory() (assetDigests, managedDirGroups []string, managedDirs bool)
}
