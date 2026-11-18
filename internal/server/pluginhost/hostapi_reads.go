package pluginhost

import (
	"io"
	"net/http"
	"slices"
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Permissions of the host API's reads.
const (
	permNodeIdentity  = "read:node_identity"
	permNodeInventory = "read:node_inventory"
	permHostIdentity  = "read:host_identity"
)

// readOwner settles who calls a read and whether it may: the handshake was
// accepted, this server answers reads, the administrator approved the
// permission and the plugin negotiated its capability. On-demand plugins
// may read; only pushes are refused to them.
func (h *hostConn) readOwner(w http.ResponseWriter, r *http.Request, permission, capability string) *Owner {
	owner, refusal := h.waitOwner(r.Context())
	switch {
	case refusal != nil:
	case h.reads == nil:
		refusal = Refuse(extapi.ReasonNotFound, messages.ErrorPluginHostRouteUnknown, "this server answers no reads",
			map[string]any{"method": r.Method, "path": r.URL.Path})
	case !slices.Contains(owner.Grant.PermissionsGranted, permission):
		refusal = Refuse(extapi.ReasonPermissionDenied, messages.ErrorPluginHostNotPermitted,
			"the plugin was not granted "+permission, map[string]any{"id": owner.ID, "permission": permission})
	case owner.Use[capability] < 1:
		refusal = Refuse(extapi.ReasonFailedPrecondition, messages.ErrorPluginCapabilityNotOffered,
			"the plugin did not negotiate "+capability+" at its handshake",
			map[string]any{"id": owner.ID, "capability": capability, "version": 1})
	}
	if refusal != nil {
		writeWire(w, refusal)
		return nil
	}
	return owner
}

// argumentInvalid answers a read whose arguments break the wire rules: a
// rule broken at a field is the plugin's argument error; a body that is not
// the object at all keeps its wire code.
func argumentInvalid(owner *Owner, err error) *Error {
	var field string
	if x, ok := err.(*extapi.Error); ok && x.Code == extapi.ErrorWireInvalid {
		field, _ = x.Params["field"].(string)
	}
	if field == "" {
		return wireRefusal(err)
	}
	return Refuse(extapi.ReasonInvalidArgument, messages.ErrorPluginHostArgumentInvalid, "invalid "+field,
		map[string]any{"id": owner.ID, "field": field})
}

// nodeIdentity answers POST /host/v1/node-identity with the salted
// identity of the named nodes, keyed with the salt bound to the package.
func (h *hostConn) nodeIdentity(w http.ResponseWriter, r *http.Request) {
	owner := h.readOwner(w, r, permNodeIdentity, extapi.CapHostNodeIdentity)
	if owner == nil {
		return
	}
	raw, ok := readBody(w, r, extapi.MaxNodeIdentityRequestBytes)
	if !ok {
		return
	}
	req, err := extapi.DecodeNodeIdentityRequest(raw)
	if err != nil {
		writeWire(w, argumentInvalid(owner, err))
		return
	}
	nodes, err := h.reads.NodeIdentity(r.Context(), req.NodeIDs, IdentityKey(owner.ID, req.Salt))
	if err != nil {
		writeWire(w, wireRefusal(err))
		return
	}
	if nodes == nil {
		nodes = []extapi.NodeIdentity{}
	}
	h.answer(w, &extapi.NodeIdentityAnswer{Nodes: nodes})
}

// readBody reads a read's request body of at most max bytes; a longer one
// is refused as too large before it is decoded.
func readBody(w http.ResponseWriter, r *http.Request, max int) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, int64(max)+1))
	if err != nil {
		writeWire(w, Refuse(extapi.ReasonCancelled, messages.ErrorPluginHostFailed, "reading the request failed",
			map[string]any{"detail": err.Error()}))
		return nil, false
	}
	if len(raw) > max {
		e := Refuse(extapi.ReasonInvalidArgument, messages.ErrorRequestTooLarge, "the request is larger than the server reads",
			map[string]any{"max": max})
		e.Status = http.StatusRequestEntityTooLarge
		writeWire(w, e)
		return nil, false
	}
	return raw, true
}

// hostIdentity answers POST /host/v1/host-identity with the salted
// identity of the server's machine, keyed with the salt bound to the
// package as the node identity is.
func (h *hostConn) hostIdentity(w http.ResponseWriter, r *http.Request) {
	owner := h.readOwner(w, r, permHostIdentity, extapi.CapHostHostIdentity)
	if owner == nil {
		return
	}
	raw, ok := readBody(w, r, extapi.MaxHostIdentityRequestBytes)
	if !ok {
		return
	}
	req, err := extapi.DecodeHostIdentityRequest(raw)
	if err != nil {
		writeWire(w, argumentInvalid(owner, err))
		return
	}
	ans, err := h.reads.HostIdentity(r.Context(), IdentityKey(owner.ID, req.Salt))
	if err != nil {
		writeWire(w, wireRefusal(err))
		return
	}
	h.answer(w, &ans)
}

// nodes answers GET /host/v1/nodes with one page of the inventory. Pages
// draw on their own bucket as well as the host API's; a query refused for
// its arguments costs no page.
func (h *hostConn) nodes(w http.ResponseWriter, r *http.Request) {
	owner := h.readOwner(w, r, permNodeInventory, extapi.CapHostNodes)
	if owner == nil {
		return
	}
	q, err := extapi.ParseNodesQuery(r.URL.Query())
	if err != nil {
		writeWire(w, argumentInvalid(owner, err))
		return
	}
	var cursor uint64
	if q.Cursor != "" {
		if cursor, err = strconv.ParseUint(q.Cursor, 10, 64); err != nil || cursor == 0 || q.Cursor != strconv.FormatUint(cursor, 10) {
			writeWire(w, Refuse(extapi.ReasonInvalidArgument, messages.ErrorPluginHostArgumentInvalid, "the cursor is not one this server gave",
				map[string]any{"id": owner.ID, "field": "cursor"}))
			return
		}
	}
	if !h.pages.allow() {
		e := Refuse(extapi.ReasonBusy, messages.ErrorPluginHostRateLimited, "the plugin reads inventory pages too fast",
			map[string]any{"limit": hostPagesPerSecond})
		e.Wire.RetryAfterMS = 1000 / hostPagesPerSecond
		writeWire(w, e)
		return
	}
	limit := q.Limit
	if limit == 0 {
		limit = extapi.MaxNodeRead
	}
	nodes, next, err := h.reads.NodeInventory(r.Context(), cursor, limit)
	if err != nil {
		writeWire(w, wireRefusal(err))
		return
	}
	if nodes == nil {
		nodes = []extapi.InventoryNode{}
	}
	page := &extapi.NodesPage{Nodes: nodes}
	if next != 0 {
		page.Next = strconv.FormatUint(next, 10)
	}
	h.answer(w, page)
}

// answer writes a read's answer after checking it against the wire rules,
// so a plugin never gets an object its SDK refuses.
func (h *hostConn) answer(w http.ResponseWriter, v interface{ Validate() error }) {
	if err := v.Validate(); err != nil {
		h.log.Error("a host API read produced an invalid answer", "plugin", h.id, "err", err)
		writeWire(w, Refuse(extapi.ReasonInternal, messages.ErrorPluginHostFailed, "invalid answer",
			map[string]any{"detail": err.Error()}))
		return
	}
	writeJSON(w, http.StatusOK, v)
}
