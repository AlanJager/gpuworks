package plugintest

import (
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The server's product codes for the reads. As with the push codes, a test
// in the server's module compares the fake host's answers with the host
// API's.
const (
	codeNotPermitted          = "PLUGIN_HOST_NOT_PERMITTED"
	codeArgumentInvalid       = "PLUGIN_HOST_ARGUMENT_INVALID"
	codeCapabilityNotOffered  = "PLUGIN_CAPABILITY_NOT_OFFERED"
	codeRequestTooLarge       = "REQUEST_TOO_LARGE"
	permNodeIdentity          = "read:node_identity"
	permNodeInventory         = "read:node_inventory"
	defaultInventoryPageLimit = extapi.MaxNodeRead
)

// handshake is what the fake host learned from the accepted initialize:
// the package, the permissions granted and the capabilities negotiated.
type handshake struct {
	id      string
	granted []string
	use     map[string]int
}

// SetNodes sets the inventory the fake host pages through, in any order;
// it serves them by ascending id.
func (h *Harness) SetNodes(nodes ...extapi.InventoryNode) {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	h.host.nodes = append([]extapi.InventoryNode(nil), nodes...)
	sort.Slice(h.host.nodes, func(i, j int) bool { return h.host.nodes[i].ID < h.host.nodes[j].ID })
}

// SetNodeIdentities sets the canned identities the fake host answers
// node-identity requests with. Unlike the server it ignores the salt: each
// named node that has a canned identity gets it, in ascending id order.
func (h *Harness) SetNodeIdentities(ids ...extapi.NodeIdentity) {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	h.host.identities = map[uint64]extapi.NodeIdentity{}
	for _, id := range ids {
		h.host.identities[id.NodeID] = id
	}
}

// readCaller checks a read the way the server does: the permission granted
// in the accepted initialize, then the capability negotiated in its answer.
func (h *fakeHost) readCaller(w http.ResponseWriter, permission, capability string) (string, bool) {
	h.mu.Lock()
	hs := h.handshake
	h.mu.Unlock()
	switch {
	case !slices.Contains(hs.granted, permission):
		writeRefusal(w, extapi.WireError{Code: codeNotPermitted, Message: "the plugin was not granted " + permission,
			Params: extapi.Params{"id": hs.id, "permission": permission}, Reason: extapi.ReasonPermissionDenied})
	case hs.use[capability] < 1:
		writeRefusal(w, extapi.WireError{Code: codeCapabilityNotOffered, Message: "the plugin did not negotiate " + capability,
			Params: extapi.Params{"id": hs.id, "capability": capability, "version": 1}, Reason: extapi.ReasonFailedPrecondition})
	default:
		return hs.id, true
	}
	return "", false
}

func argumentRefusal(id string, err error) extapi.WireError {
	if e, ok := err.(*extapi.Error); ok && e.Code == extapi.ErrorWireInvalid {
		if field, _ := e.Params["field"].(string); field != "" {
			return argumentInvalid(id, field)
		}
	}
	return *wireOf(err, extapi.ReasonInvalidArgument)
}

func argumentInvalid(id, field string) extapi.WireError {
	return extapi.WireError{Code: codeArgumentInvalid, Message: "invalid " + field,
		Params: extapi.Params{"id": id, "field": field}, Reason: extapi.ReasonInvalidArgument}
}

func (h *fakeHost) nodeIdentity(w http.ResponseWriter, r *http.Request) {
	id, ok := h.readCaller(w, permNodeIdentity, extapi.CapHostNodeIdentity)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, extapi.MaxNodeIdentityRequestBytes+1))
	if err != nil {
		writeRefusal(w, *wireOf(err, extapi.ReasonInvalidArgument))
		return
	}
	if len(raw) > extapi.MaxNodeIdentityRequestBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, extapi.WireError{Code: codeRequestTooLarge,
			Message: "the request is larger than the server reads",
			Params:  extapi.Params{"max": extapi.MaxNodeIdentityRequestBytes}, Reason: extapi.ReasonInvalidArgument})
		return
	}
	req, err := extapi.DecodeNodeIdentityRequest(raw)
	if err != nil {
		writeRefusal(w, argumentRefusal(id, err))
		return
	}
	out := extapi.NodeIdentityAnswer{Nodes: []extapi.NodeIdentity{}}
	h.mu.Lock()
	for _, n := range slices.Sorted(slices.Values(req.NodeIDs)) {
		if ident, ok := h.identities[n]; ok {
			out.Nodes = append(out.Nodes, ident)
		}
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (h *fakeHost) inventory(w http.ResponseWriter, r *http.Request) {
	id, ok := h.readCaller(w, permNodeInventory, extapi.CapHostNodes)
	if !ok {
		return
	}
	q, err := extapi.ParseNodesQuery(r.URL.Query())
	if err != nil {
		writeRefusal(w, argumentRefusal(id, err))
		return
	}
	var after uint64
	if q.Cursor != "" {
		if after, err = strconv.ParseUint(q.Cursor, 10, 64); err != nil || after == 0 || q.Cursor != strconv.FormatUint(after, 10) {
			writeRefusal(w, argumentInvalid(id, "cursor"))
			return
		}
	}
	limit := q.Limit
	if limit == 0 {
		limit = defaultInventoryPageLimit
	}
	page := extapi.NodesPage{Nodes: []extapi.InventoryNode{}}
	h.mu.Lock()
	rest := 0
	for _, n := range h.nodes {
		if n.ID <= after {
			continue
		}
		if len(page.Nodes) == limit {
			rest++
			break
		}
		page.Nodes = append(page.Nodes, n)
	}
	h.mu.Unlock()
	if rest > 0 {
		page.Next = strconv.FormatUint(page.Nodes[len(page.Nodes)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, page)
}
