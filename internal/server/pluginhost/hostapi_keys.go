package pluginhost

import (
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// deriveKey answers POST /host/v1/derive-key with a key bound to the caller's
// package and the label.
func (h *hostConn) deriveKey(w http.ResponseWriter, r *http.Request) {
	owner := h.callOwner(w, r, h.svc != nil && h.svc.keys != nil, extapi.CapHostDeriveKey, needsPermission(extapi.PermCryptoDeriveKey))
	if owner == nil {
		return
	}
	raw, ok := callBody(w, r)
	if !ok {
		return
	}
	req, err := extapi.DecodeKeyRequest(raw)
	if err != nil {
		writeWire(w, badArgument(owner, err))
		return
	}
	key, err := h.svc.keys.DeriveKey(*owner, req.Label, req.Length)
	if err != nil {
		writeWire(w, wireRefusal(err))
		return
	}
	if len(key) != req.Length {
		h.log.Error("the key deriver answered a key of another length", "plugin", h.id, "want", req.Length, "got", len(key))
		writeWire(w, Refuse(extapi.ReasonInternal, messages.ErrorPluginHostFailed, "the derived key has the wrong length",
			map[string]any{"detail": "length"}))
		return
	}
	h.answerChecked(w, &extapi.DerivedKey{Key: key})
}
