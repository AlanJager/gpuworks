package pluginhost

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// hostServices are the host API's calls beside pushes: the clock, $/ready,
// own state and derived keys, with what the meta answer names.
type hostServices struct {
	clock   ClockHost
	ready   ReadyHost
	own     OwnState
	keys    KeyDeriver
	bootID  string
	version func() string
}

func newHostServices(cfg *Config) (*hostServices, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("pluginhost: boot id: %w", err)
	}
	core := cfg.Core
	return &hostServices{clock: cfg.ExtClock, ready: cfg.Ready, own: cfg.OwnState, keys: cfg.Keys,
		bootID: hex.EncodeToString(id[:]), version: func() string { return core().Version }}, nil
}

// callOwner settles who makes a call and whether it may: the handshake was
// accepted, this server serves the endpoint (present), allowed (the
// endpoint's permission or role) passes, and the plugin negotiated
// capability at its handshake. It answers the refusal itself and returns
// nil when the call may not go on.
func (h *hostConn) callOwner(w http.ResponseWriter, r *http.Request, present bool, capability string, allowed func(*Owner) *Error) *Owner {
	owner, refusal := h.waitOwner(r.Context())
	switch {
	case refusal != nil:
	case !present:
		refusal = Refuse(extapi.ReasonNotFound, messages.ErrorPluginHostRouteUnknown, "this server does not serve this endpoint",
			map[string]any{"method": r.Method, "path": r.URL.Path})
	default:
		if allowed != nil {
			refusal = allowed(owner)
		}
		if refusal == nil && owner.Use[capability] < 1 {
			refusal = Refuse(extapi.ReasonFailedPrecondition, messages.ErrorPluginCapabilityNotOffered,
				"the plugin did not negotiate "+capability+" at its handshake",
				map[string]any{"id": owner.ID, "capability": capability, "version": 1})
		}
	}
	if refusal != nil {
		writeWire(w, refusal)
		return nil
	}
	return owner
}

// needsRole lets a call through only for a package granted role.
func needsRole(role string) func(*Owner) *Error {
	return func(o *Owner) *Error {
		if slices.Contains(o.Grant.RolesGranted, role) {
			return nil
		}
		return Refuse(extapi.ReasonPermissionDenied, messages.ErrorPluginRoleRequired,
			"the plugin needs the "+role+" role", map[string]any{"id": o.ID, "role": role})
	}
}

// needsPermission lets a call through only for a package whose
// administrator approved permission.
func needsPermission(permission string) func(*Owner) *Error {
	return func(o *Owner) *Error {
		if slices.Contains(o.Grant.PermissionsGranted, permission) {
			return nil
		}
		return Refuse(extapi.ReasonPermissionDenied, messages.ErrorPluginHostNotPermitted,
			"the plugin was not granted "+permission, map[string]any{"id": o.ID, "permission": permission})
	}
}

// callBody reads a small request body, refusing one larger than
// extapi.MaxHostCallBytes as too large.
func callBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, extapi.MaxHostCallBytes+1))
	if err != nil {
		writeWire(w, Refuse(extapi.ReasonCancelled, messages.ErrorPluginHostFailed, "reading the request failed",
			map[string]any{"detail": err.Error()}))
		return nil, false
	}
	if len(raw) > extapi.MaxHostCallBytes {
		e := Refuse(extapi.ReasonInvalidArgument, messages.ErrorRequestTooLarge, "the request is larger than the server reads",
			map[string]any{"max": extapi.MaxHostCallBytes})
		e.Status = http.StatusRequestEntityTooLarge
		writeWire(w, e)
		return nil, false
	}
	return raw, true
}

// badArgument answers a request whose body breaks the wire rules: a rule
// broken at a field is the plugin's argument error; a body that is not the
// object at all keeps its wire code.
func badArgument(owner *Owner, err error) *Error {
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

// answerChecked writes an answer after checking it against the wire rules,
// so a plugin never gets an object its SDK refuses.
func (h *hostConn) answerChecked(w http.ResponseWriter, v interface{ Validate() error }) {
	if err := v.Validate(); err != nil {
		h.log.Error("a host API call produced an invalid answer", "plugin", h.id, "err", err)
		writeWire(w, Refuse(extapi.ReasonInternal, messages.ErrorPluginHostFailed, "invalid answer",
			map[string]any{"detail": err.Error()}))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// declareReady answers POST /host/v1/state/ready: the process has finished
// starting. It is answered 204 whatever it changed: a second call, one from
// an on-demand plugin (which has no boot extension, as it pushes nothing)
// and one on a server without pushed state change nothing.
func (h *hostConn) declareReady(w http.ResponseWriter, r *http.Request) {
	owner, refusal := h.waitOwner(r.Context())
	if refusal != nil {
		writeWire(w, refusal)
		return
	}
	if owner.Lifecycle != extapi.LifecycleOnDemand && h.svc != nil && h.svc.ready != nil {
		h.svc.ready.Ready(*owner)
	}
	w.WriteHeader(http.StatusNoContent)
}

// meta answers GET /host/v1/meta.
func (h *hostConn) meta(w http.ResponseWriter, r *http.Request) {
	if h.callOwner(w, r, h.svc != nil && h.svc.clock != nil, extapi.CapHostClock, nil) == nil {
		return
	}
	wall, ext, hwm := h.svc.clock.Clocks()
	m := &extapi.HostMeta{Version: h.svc.version(), BootID: h.svc.bootID, Wall: extapi.NewTime(wall), ExtClock: extapi.NewTime(ext)}
	if !hwm.IsZero() {
		t := extapi.NewTime(hwm)
		m.HWM = &t
	}
	h.answerChecked(w, m)
}

// correctClock answers POST /host/v1/clock/correct for a package granted
// the clock_authority role.
func (h *hostConn) correctClock(w http.ResponseWriter, r *http.Request) {
	owner := h.callOwner(w, r, h.svc != nil && h.svc.clock != nil, extapi.CapHostClock, needsRole(extapi.RoleClockAuthority))
	if owner == nil {
		return
	}
	raw, ok := callBody(w, r)
	if !ok {
		return
	}
	req, err := extapi.DecodeClockCorrection(raw)
	if err != nil {
		writeWire(w, badArgument(owner, err))
		return
	}
	from, to, err := h.svc.clock.Correct(r.Context(), *owner, req.To.Time, req.Reason)
	if err != nil {
		writeWire(w, wireRefusal(err))
		return
	}
	h.answerChecked(w, &extapi.ClockCorrected{From: extapi.NewTime(from), To: extapi.NewTime(to)})
}

// ownState answers GET /host/v1/state/{kind} with the caller's own table of
// kind.
func (h *hostConn) ownState(w http.ResponseWriter, r *http.Request, kind string) {
	owner := h.callOwner(w, r, h.svc != nil && h.svc.own != nil, extapi.CapHostStateRead, nil)
	if owner == nil {
		return
	}
	if probe := (extapi.PushedState{Kind: kind}); probe.Validate() != nil {
		writeWire(w, Refuse(extapi.ReasonInvalidArgument, messages.ErrorPluginHostArgumentInvalid, "the path names no table kind",
			map[string]any{"id": owner.ID, "field": "kind"}))
		return
	}
	held, err := h.svc.own.OwnRows(*owner, kind)
	if err != nil {
		writeWire(w, wireRefusal(err))
		return
	}
	h.answerChecked(w, &held)
}
