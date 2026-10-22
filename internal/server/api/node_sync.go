package api

// NodeSync separates full desired-state updates from asynchronous network-only
// notifications. Saving proxy settings must not wait on an agent's control stream.
type NodeSync interface {
	SyncNode(uuid string) error
	RequestNetworkSync(uuid string)
}

func (a *apiHandlers) requestNetworkSync(uuid string) {
	if a.d.NodeSync != nil && (a.d.Online == nil || a.d.Online(uuid)) {
		a.d.NodeSync.RequestNetworkSync(uuid)
	}
}
