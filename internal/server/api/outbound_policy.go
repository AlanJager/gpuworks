package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// OutboundPolicyManager is separate from the runtime permission reader: callers
// that supply read-only policies need not gain configuration-writing authority.
type OutboundPolicyManager interface {
	OutboundPolicy() (dto.ServerOutboundPolicy, error)
	UpdateOutboundPolicy(dto.ServerOutboundPolicyUpdate) (dto.ServerOutboundPolicy, error)
}

func (a *apiHandlers) getServerOutboundPolicy(w http.ResponseWriter, r *http.Request) {
	manager, ok := a.serverSettings().(OutboundPolicyManager)
	if !ok {
		writeJSON(w, http.StatusOK, dto.ServerOutboundPolicy{AllowPrivateHosts: []string{}})
		return
	}
	policy, err := manager.OutboundPolicy()
	if err != nil {
		writeOutboundPolicyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (a *apiHandlers) putServerOutboundPolicy(w http.ResponseWriter, r *http.Request) {
	var update dto.ServerOutboundPolicyUpdate
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&update)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = errors.New("trailing JSON")
		}
	}
	if err != nil || update.RestrictPrivateHosts == nil || update.AllowPrivateHosts == nil || update.Revision == "" {
		writeProductError(w, http.StatusBadRequest, ErrorServerOutboundPolicyInvalid, "provide a restriction mode, a private-host list and the current revision", nil)
		return
	}
	manager, ok := a.serverSettings().(OutboundPolicyManager)
	if !ok {
		writeProductError(w, http.StatusConflict, ErrorServerOutboundPolicyReadOnly, "the server was started without an editable configuration file", nil)
		return
	}
	policy, err := manager.UpdateOutboundPolicy(update)
	if err != nil {
		writeOutboundPolicyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func writeOutboundPolicyError(w http.ResponseWriter, err error) {
	var setting *ServerSettingsError
	if errors.As(err, &setting) {
		status := http.StatusBadRequest
		switch setting.Code {
		case ErrorServerOutboundPolicyConflict, ErrorServerOutboundPolicyReadOnly:
			status = http.StatusConflict
		case ErrorServerOutboundPolicyUnavailable:
			status = http.StatusServiceUnavailable
		}
		writeProductError(w, status, setting.Code, setting.Fallback, setting.Params)
		return
	}
	// YAML parser and filesystem errors may contain credentials or private paths.
	writeProductError(w, http.StatusServiceUnavailable, ErrorServerOutboundPolicyUnavailable, "the outbound policy configuration could not be read or saved", nil)
}
