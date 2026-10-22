package planning

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A group's collectives run in the library its accelerators bring, and that
// library decides which kind of network can carry them. Accelerators whose
// cards own network ports (accelerator.CardNetworkOf: Ascend) move data
// between hosts only on those ports: there is no switch that sends it over a
// host interface and no fallback, so a group that starts on any other network
// fails at its first collective (docs/adr/0024-network-objects.md).

// CheckGroupNetwork reports why the network a multi-node group shares cannot
// carry its collectives: nodes whose cards own network ports need an
// accelerator RDMA network, a node on one must have a card on it, and a host
// RDMA network needs a runtime that can hand the verbs devices to a
// container, which udocker cannot. A single node has no collective across
// hosts and always passes.
func CheckGroupNetwork(nodes []store.Node, group netplan.Group) error {
	if len(nodes) < 2 {
		return nil
	}
	onCards := group.Network.Type == netplan.TypeAcceleratorRDMA
	for _, node := range nodes {
		if _, cards := accelerator.CardNetworkOf(node.GPUVendor); cards && !onCards {
			return messages.NewProductError(messages.ErrorComputeNetworkAcceleratorRequired,
				fmt.Sprintf("node %s runs %s accelerators, whose collectives across hosts need an accelerator RDMA network; the group shares %s", node.DisplayName(), accelerator.NormalizeVendor(node.GPUVendor), group.Network.Name),
				map[string]any{"node": node.DisplayName(), "network": group.Network.Name})
		}
		if err := checkCards(node, group); err != nil {
			return err
		}
		if group.Network.Type == netplan.TypeHostRDMA && node.RuntimeDriver == "udocker" {
			return messages.NewProductError(messages.ErrorComputeNetworkRuntime,
				fmt.Sprintf("node %s runs udocker, which cannot expose RDMA devices for compute network %s", node.DisplayName(), group.Network.Name),
				map[string]any{"node": node.DisplayName(), "network": group.Network.Name, "runtime": node.RuntimeDriver})
		}
	}
	return nil
}

// CheckPDNetwork reports why the network a PD group shares cannot carry its
// KV transfer in this mode. A mode whose transport is HCCL needs an
// accelerator RDMA network as soon as the group spans hosts; a mode whose
// transport is UCX cannot use one, because UCX drives host devices. A
// sleepable deployment cannot move its KV over UCX on a host RDMA network:
// NIXL registers the engine's KV cache with the NIC, and once the engine has
// slept it cannot allocate that memory again (wake_up: CUDA out of memory,
// measured with dma-buf registration; collectives over the same NIC sleep
// fine, they register buffers of their own).
func CheckPDNetwork(mode pdmode.Mode, nodes []store.Node, group netplan.Group, sleepable bool) error {
	onCards := group.Network.Type == netplan.TypeAcceleratorRDMA
	params := map[string]any{"mode": mode.Name, "network": group.Network.Name}
	switch {
	case mode.Transport == pdmode.TransportHCCL && len(nodes) > 1 && !onCards:
		return messages.NewProductError(messages.ErrorPDNetworkAcceleratorRequired,
			fmt.Sprintf("PD mode %s moves KV on the accelerator cards' ports, so a group across hosts needs an accelerator RDMA network; the group shares %s", mode.Name, group.Network.Name), params)
	case mode.Transport == pdmode.TransportUCX && onCards:
		return messages.NewProductError(messages.ErrorPDNetworkHostRequired,
			fmt.Sprintf("PD mode %s moves KV over UCX, which needs a host network; the group shares accelerator RDMA network %s", mode.Name, group.Network.Name), params)
	case sleepable && mode.Transport == pdmode.TransportUCX && group.Network.Type == netplan.TypeHostRDMA:
		return messages.NewProductError(messages.ErrorPDSleepRDMAUnsupported,
			fmt.Sprintf("a sleepable PD deployment cannot move KV over RDMA network %s: the KV cache registered with the NIC cannot be allocated again after sleep; turn sleep off or use an Ethernet compute network", group.Network.Name), params)
	}
	if len(nodes) > 1 {
		for _, node := range nodes {
			if err := checkCards(node, group); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCards refuses a node that is on an accelerator RDMA network without a
// usable card on it.
func checkCards(node store.Node, group netplan.Group) error {
	if group.Network.Type != netplan.TypeAcceleratorRDMA || len(group.Ranks[node.ID].HCAs) > 0 {
		return nil
	}
	return messages.NewProductError(messages.ErrorComputeNetworkNoCards,
		fmt.Sprintf("node %s has no accelerator card on compute network %s", node.DisplayName(), group.Network.Name),
		map[string]any{"node": node.DisplayName(), "network": group.Network.Name})
}
