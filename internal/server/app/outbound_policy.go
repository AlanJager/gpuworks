package app

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

var _ api.OutboundPolicyManager = (*serverSettings)(nil)

func (s *serverSettings) OutboundPolicy() (dto.ServerOutboundPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outboundPolicyLocked()
}

func (s *serverSettings) outboundPolicyLocked() (dto.ServerOutboundPolicy, error) {
	out := dto.ServerOutboundPolicy{RestrictPrivateHosts: s.cfg.OutboundRestrictPrivateHosts, AllowPrivateHosts: append([]string{}, s.cfg.OutboundAllowPrivateHosts...), Editable: s.cfg.ConfigPath != ""}
	if !out.Editable {
		return out, nil
	}
	restrict, hosts, revision, err := serverconfig.ReadOutboundPolicy(s.cfg.ConfigPath)
	if err != nil {
		return dto.ServerOutboundPolicy{}, &api.ServerSettingsError{Code: api.ErrorServerOutboundPolicyUnavailable, Fallback: "the outbound policy configuration could not be read"}
	}
	out.RestrictPrivateHosts, out.AllowPrivateHosts, out.Revision = restrict, append([]string{}, hosts...), revision
	return out, nil
}

func (s *serverSettings) UpdateOutboundPolicy(update dto.ServerOutboundPolicyUpdate) (dto.ServerOutboundPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.ConfigPath == "" {
		return dto.ServerOutboundPolicy{}, &api.ServerSettingsError{Code: api.ErrorServerOutboundPolicyReadOnly, Fallback: "the server was started without an editable configuration file"}
	}
	if update.RestrictPrivateHosts == nil || update.AllowPrivateHosts == nil || update.Revision == "" {
		return dto.ServerOutboundPolicy{}, &api.ServerSettingsError{Code: api.ErrorServerOutboundPolicyInvalid, Fallback: "provide a restriction mode, a private-host list and the current revision"}
	}
	hosts, err := serverconfig.NormalizeOutboundPrivateHosts(update.AllowPrivateHosts)
	if err != nil {
		return dto.ServerOutboundPolicy{}, &api.ServerSettingsError{Code: api.ErrorServerOutboundPolicyInvalid, Fallback: "enter valid hostnames, IP addresses or CIDR ranges"}
	}
	revision, err := serverconfig.SaveOutboundPolicy(s.cfg.ConfigPath, *update.RestrictPrivateHosts, hosts, update.Revision)
	if err != nil {
		return dto.ServerOutboundPolicy{}, outboundPolicyError(err)
	}
	// Return the committed policy. Another administrator may change the file
	// immediately afterwards; their next read will then expose a newer revision.
	return dto.ServerOutboundPolicy{RestrictPrivateHosts: *update.RestrictPrivateHosts, AllowPrivateHosts: append([]string{}, hosts...), Editable: true, Revision: revision}, nil
}

func outboundPolicyError(err error) error {
	switch {
	case errors.Is(err, serverconfig.ErrOutboundPolicyConflict):
		return &api.ServerSettingsError{Code: api.ErrorServerOutboundPolicyConflict, Fallback: "the outbound policy changed; reload it before saving"}
	case errors.Is(err, serverconfig.ErrOutboundPolicyInvalid):
		return &api.ServerSettingsError{Code: api.ErrorServerOutboundPolicyInvalid, Fallback: "enter valid hostnames, IP addresses or CIDR ranges"}
	default:
		return &api.ServerSettingsError{Code: api.ErrorServerOutboundPolicyUnavailable, Fallback: "the outbound policy configuration could not be read or saved"}
	}
}
