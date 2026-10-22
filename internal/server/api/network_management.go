package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The management network is not a stored network: every node is on it at the
// address its agent reaches the server from. It is listed as network 0 so the
// network pages can show it; only its per-node roles can be changed.

const managementNetworkID = "0"

func isManagementNetworkID(id string) bool { return id == managementNetworkID }

func (st networkState) managementNetworkDTO() dto.Network {
	n := netplan.Management()
	out := dto.Network{
		Name: n.Name, Roles: nonNilStrings(n.Roles), Type: n.Type,
		Builtin: true, Attachments: []dto.NetworkAttachment{},
	}
	resolvedNodes, warned := map[uint]bool{}, map[uint]bool{}
	for _, node := range st.Nodes {
		if managementIP(node) != "" {
			resolvedNodes[node.ID] = true
		}
	}
	for _, r := range st.Resolved {
		if r.IsManagement() && len(r.Warnings) > 0 {
			warned[r.NodeID] = true
		}
	}
	out.ResolvedNodeCount, out.WarningNodeCount = len(resolvedNodes), len(warned)
	return out
}

// managementDetail lists every node that reported a management address, with
// the roles the management network actually carries there: a role a
// dedicated network covers, or one switched off, is not among them.
func (st networkState) managementDetail(latest *store.NetworkTestRun) dto.NetworkDetail {
	out := dto.NetworkDetail{Network: st.managementNetworkDTO(), Nodes: []dto.NetworkNode{}, Roles: netplan.Roles, Types: netplan.Types, TestSupported: true}
	out.LatestTest = networkTestDTO(latest, st.nodeByID())
	carried := map[uint][]string{}
	warnings := map[uint][]dto.NetworkWarning{}
	for _, r := range st.Resolved {
		if r.IsManagement() {
			carried[r.NodeID] = append(carried[r.NodeID], r.Role)
			warnings[r.NodeID] = append(warnings[r.NodeID], warningDTOs(r.Warnings)...)
		}
	}
	for _, node := range st.Nodes {
		addr, ok := network.ManagementAddress(node)
		if !ok {
			continue
		}
		out.Nodes = append(out.Nodes, dto.NetworkNode{
			NodeUUID: node.UUID, NodeName: nodeDisplayName(&node), Labels: writtenLabels(&node), Roles: nonNilStrings(carried[node.ID]),
			Source: netplan.SourceManagement, Interface: addr.Interface, IP: addr.IP, HCAs: []dto.NetworkRail{},
			Tools: nodeTools(node), Warnings: nonNilWarnings(warnings[node.ID]), RolesOff: managementRolesOff(node),
		})
	}
	sort.SliceStable(out.Nodes, func(i, j int) bool { return out.Nodes[i].NodeName < out.Nodes[j].NodeName })
	return out
}

func nonNilWarnings(ws []dto.NetworkWarning) []dto.NetworkWarning {
	if ws == nil {
		return []dto.NetworkWarning{}
	}
	return ws
}

func managementRolesOff(node store.Node) []string {
	return nonNilStrings(network.RolesOff(node))
}

// nodeManagementDTO carries the node's switched-off roles even before its
// agent reports a management address; Interface and IP are empty until then.
func nodeManagementDTO(node store.Node) *dto.NodeManagement {
	out := &dto.NodeManagement{RolesOff: managementRolesOff(node)}
	addr, ok := network.ManagementAddress(node)
	if !ok {
		return out
	}
	out.Interface, out.IP = addr.Interface, addr.IP
	if remote := remoteHost(node.LastRemoteAddr); remote != "" {
		out.RemoteAddr = remote
		// A connection relayed on the server host (server_dial over SSH, a
		// local proxy) says nothing about NAT.
		if ip := net.ParseIP(remote); ip != nil && !ip.IsLoopback() {
			out.BehindNAT = !ip.Equal(net.ParseIP(addr.IP))
		}
	}
	return out
}

func remoteHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return host
	}
	return raw
}

// patchManagementRoles changes the named roles on many nodes at once, all or
// nothing. The UI selects the nodes by search or label; the server only sees
// the resulting list.
func (a *apiHandlers) patchManagementRoles(w http.ResponseWriter, r *http.Request) {
	var req dto.ManagementRolesBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.NodeUUIDs) == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorManagementRolesNodesRequired, "select at least one node", nil)
		return
	}
	updated, err := a.networks().UpdateManagementRoles(req.NodeUUIDs, req.Allow, req.Deny)
	var missing *store.ManagementRolesNotFoundError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, dto.ManagementRolesBatchResult{Updated: updated})
	case errors.Is(err, netplan.ErrUnknownRole):
		writeProductError(w, http.StatusBadRequest, ErrorNetworkRoleInvalid, "unknown network role", nil)
	case errors.Is(err, network.ErrManagementRolesConflict):
		writeProductError(w, http.StatusBadRequest, ErrorManagementRolesConflict, "a role cannot be both allowed and denied", nil)
	case errors.As(err, &missing):
		writeProductError(w, http.StatusNotFound, ErrorManagementRolesNodeNotFound, "node not found", map[string]any{"node": missing.UUID})
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// putNodeManagementRoles replaces the roles a node's management network may
// not carry and answers with the node's networks. Running instances keep
// the network they started on.
func (a *apiHandlers) putNodeManagementRoles(w http.ResponseWriter, r *http.Request) {
	uuid := chi.URLParam(r, "uuid")
	var req dto.ManagementRolesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.networks().SetManagementRolesOff(uuid, req.RolesOff); err != nil {
		switch {
		case errors.Is(err, netplan.ErrUnknownRole):
			writeProductError(w, http.StatusBadRequest, ErrorNetworkRoleInvalid, "unknown network role", nil)
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	a.getNodeNetworks(w, r)
}
