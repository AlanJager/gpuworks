package plugintest

import (
	"crypto/hkdf"
	"crypto/sha256"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The server's codes for the clock, own-state and key calls. As with the
// push codes, a test in the server's module compares the fake host's
// answers with the host API's.
const (
	hostCodeNotPermitted     = "PLUGIN_HOST_NOT_PERMITTED"
	hostCodeArgumentInvalid  = "PLUGIN_HOST_ARGUMENT_INVALID"
	hostCodeNotNegotiated    = "PLUGIN_CAPABILITY_NOT_OFFERED"
	hostCodeRoleRequired     = "PLUGIN_ROLE_REQUIRED"
	hostCodeRequestTooLarge  = "REQUEST_TOO_LARGE"
	hostCodeCorrectionTooFar = "PLUGIN_CLOCK_CORRECTION_TOO_LARGE"
	// maxCorrectionDays is the server's bound on one clock correction.
	maxCorrectionDays = 366
)

// fakeMasterKey stands in for the server's master key: the fake host
// derives keys the way the server does, from this key, so a key is
// deterministic per package and label but not the one a real server gives.
var fakeMasterKey = []byte("plugintest master key")

// hostCalls is the fake host's record of the clock, ready, own-state and
// key calls; guarded by fakeHost.mu.
type hostCalls struct {
	// accepted is what the accepted initialize granted and negotiated.
	accepted    acceptedInit
	readies     int
	corrections []extapi.ClockCorrection
	// tables are the tables pushed to the fake host (or set), by kind.
	tables map[string]extapi.PushedState
}

type acceptedInit struct {
	id      string
	granted []string
	roles   []string
	use     map[string]int
}

func (c *hostCalls) keep(t extapi.PushedState) {
	if c.tables == nil {
		c.tables = map[string]extapi.PushedState{}
	}
	c.tables[t.Kind] = t
}

func (h *fakeHost) acceptInit(req *extapi.HandshakeRequest, resp *extapi.HandshakeResponse) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls.accepted = acceptedInit{id: req.Plugin.ID, granted: slices.Clone(req.Plugin.PermissionsGranted),
		roles: slices.Clone(req.Plugin.RolesGranted), use: maps.Clone(resp.Use)}
}

// Readies is how many times the plugin declared itself ready.
func (h *Harness) Readies() int {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	return h.host.calls.readies
}

// Corrections are the clock corrections the fake host accepted, in order.
func (h *Harness) Corrections() []extapi.ClockCorrection {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	return slices.Clone(h.host.calls.corrections)
}

// SetPushedState makes the fake host hold t (its Kind) for the plugin's
// own-state reads and its handshake, as a server that kept a table across a
// restart of the plugin does.
func (h *Harness) SetPushedState(t extapi.PushedState) {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	h.host.calls.keep(t)
	h.host.held[t.Kind] = extapi.HeldState{Kind: t.Kind, Schema: t.Schema, Generation: t.Generation, Digest: t.Digest}
}

func (h *fakeHost) ready(w http.ResponseWriter) {
	h.mu.Lock()
	h.calls.readies++
	h.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// caller checks a call the way the server does: a permission or role
// granted in the accepted initialize, then the capability negotiated in its
// answer.
func (h *fakeHost) caller(w http.ResponseWriter, permission, role, capability string) (string, bool) {
	h.mu.Lock()
	a := h.calls.accepted
	h.mu.Unlock()
	var refusal *extapi.WireError
	switch {
	case permission != "" && !slices.Contains(a.granted, permission):
		refusal = &extapi.WireError{Code: hostCodeNotPermitted, Message: "the plugin was not granted " + permission,
			Params: extapi.Params{"id": a.id, "permission": permission}, Reason: extapi.ReasonPermissionDenied}
	case role != "" && !slices.Contains(a.roles, role):
		refusal = &extapi.WireError{Code: hostCodeRoleRequired, Message: "the plugin needs the " + role + " role",
			Params: extapi.Params{"id": a.id, "role": role}, Reason: extapi.ReasonPermissionDenied}
	case a.use[capability] < 1:
		refusal = &extapi.WireError{Code: hostCodeNotNegotiated, Message: "the plugin did not negotiate " + capability + " at its handshake",
			Params: extapi.Params{"id": a.id, "capability": capability, "version": 1}, Reason: extapi.ReasonFailedPrecondition}
	}
	if refusal != nil {
		writeRefusal(w, *refusal)
		return "", false
	}
	return a.id, true
}

func hostArgument(id string, err error) extapi.WireError {
	if x, ok := err.(*extapi.Error); ok && x.Code == extapi.ErrorWireInvalid {
		if field, _ := x.Params["field"].(string); field != "" {
			return extapi.WireError{Code: hostCodeArgumentInvalid, Message: "invalid " + field,
				Params: extapi.Params{"id": id, "field": field}, Reason: extapi.ReasonInvalidArgument}
		}
	}
	return *wireOf(err, extapi.ReasonInvalidArgument)
}

// hostCallBody reads a call's body as the server does: at most
// extapi.MaxHostCallBytes.
func hostCallBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, extapi.MaxHostCallBytes+1))
	if err == nil && len(raw) <= extapi.MaxHostCallBytes {
		return raw, true
	}
	writeJSON(w, http.StatusRequestEntityTooLarge, extapi.WireError{Code: hostCodeRequestTooLarge, Message: "the request is larger than the server reads",
		Params: extapi.Params{"max_bytes": extapi.MaxHostCallBytes}, Reason: extapi.ReasonInvalidArgument})
	return nil, false
}

// hostCall answers meta, clock correction and key derivation.
func (h *fakeHost) hostCall(w http.ResponseWriter, r *http.Request) {
	want := map[string]string{extapi.PathHostMeta: http.MethodGet, extapi.PathHostClockCorrect: http.MethodPost, extapi.PathHostDeriveKey: http.MethodPost}
	if r.Method != want[r.URL.Path] {
		writeRefusal(w, routeUnknown(r, extapi.ReasonUnsupported))
		return
	}
	switch r.URL.Path {
	case extapi.PathHostMeta:
		if _, ok := h.caller(w, "", "", extapi.CapHostClock); !ok {
			return
		}
		now := extapi.NewTime(h.now())
		writeJSON(w, http.StatusOK, extapi.HostMeta{Version: "v0.0.0-plugintest", BootID: "00000000000000000000000000000001", Wall: now, ExtClock: now})
	case extapi.PathHostClockCorrect:
		h.correct(w, r)
	case extapi.PathHostDeriveKey:
		h.deriveKey(w, r)
	}
}

func (h *fakeHost) correct(w http.ResponseWriter, r *http.Request) {
	id, ok := h.caller(w, "", extapi.RoleClockAuthority, extapi.CapHostClock)
	if !ok {
		return
	}
	raw, ok := hostCallBody(w, r)
	if !ok {
		return
	}
	req, err := extapi.DecodeClockCorrection(raw)
	if err != nil {
		writeRefusal(w, hostArgument(id, err))
		return
	}
	now := h.now()
	if d := req.To.Sub(now); d > maxCorrectionDays*24*time.Hour || d < -maxCorrectionDays*24*time.Hour {
		writeRefusal(w, extapi.WireError{Code: hostCodeCorrectionTooFar, Message: "a correction moves extclock by at most 366 days",
			Params: extapi.Params{"max_days": maxCorrectionDays}, Reason: extapi.ReasonInvalidArgument})
		return
	}
	h.mu.Lock()
	h.calls.corrections = append(h.calls.corrections, *req)
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, extapi.ClockCorrected{From: extapi.NewTime(now), To: req.To})
}

func (h *fakeHost) deriveKey(w http.ResponseWriter, r *http.Request) {
	id, ok := h.caller(w, extapi.PermCryptoDeriveKey, "", extapi.CapHostDeriveKey)
	if !ok {
		return
	}
	raw, ok := hostCallBody(w, r)
	if !ok {
		return
	}
	req, err := extapi.DecodeKeyRequest(raw)
	if err != nil {
		writeRefusal(w, hostArgument(id, err))
		return
	}
	key, err := hkdf.Key(sha256.New, fakeMasterKey, nil, "gpuworks plugin key\x00"+id+"\x00"+req.Label, req.Length)
	if err != nil {
		writeRefusal(w, *wireOf(err, extapi.ReasonInternal))
		return
	}
	writeJSON(w, http.StatusOK, extapi.DerivedKey{Key: key})
}

func (h *fakeHost) ownState(w http.ResponseWriter, kind string) {
	id, ok := h.caller(w, "", "", extapi.CapHostStateRead)
	if !ok {
		return
	}
	if !slices.Contains(extapi.TableKinds(), kind) {
		writeRefusal(w, extapi.WireError{Code: hostCodeArgumentInvalid, Message: "the path names no table kind",
			Params: extapi.Params{"id": id, "field": "kind"}, Reason: extapi.ReasonInvalidArgument})
		return
	}
	h.mu.Lock()
	t, held := h.calls.tables[kind]
	h.mu.Unlock()
	if !held {
		t = extapi.PushedState{Kind: kind}
	}
	writeJSON(w, http.StatusOK, t)
}
