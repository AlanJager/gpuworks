package hub

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Networks answers the hub's network questions; the app wires
// internal/server/network's Service in.
type Networks interface {
	// StoragePath picks the network a direct copy from source to target uses.
	StoragePath(sourceID, targetID uint) (netplan.StoragePath, error)
	// ServingIP is where a node runs its serving forwarder, "" for none.
	ServingIP(nodeID uint) string
}

// SetNetworks replaces the network service. The app passes the one every
// component shares, so they all read the same catalog.
func (h *Hub) SetNetworks(n Networks) { h.networks = n }

// storagePath is the network a direct copy from source to target uses. Two
// nodes with none cannot copy from each other; the target downloads from the
// source registry or model hub instead.
func (h *Hub) storagePath(source, target *store.Node) (netplan.StoragePath, error) {
	path, err := h.networks.StoragePath(source.ID, target.ID)
	if errors.Is(err, netplan.ErrNoStoragePath) {
		return path, noStoragePathError(source, target)
	}
	return path, err
}

// noStoragePathError is the product error for two nodes without a shared
// storage-sync network.
func noStoragePathError(source, target *store.Node) error {
	return messages.NoStoragePath(source.DisplayName(), target.DisplayName())
}
