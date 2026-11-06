package app

import (
	"errors"
	"log/slog"
	"os"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func privateHostPermissions(cfg Config, log *slog.Logger) func() []string {
	var report func(error)
	if log != nil {
		report = func(err error) {
			reason := "invalid_or_changed"
			if errors.Is(err, os.ErrNotExist) {
				reason = "missing"
			} else if errors.Is(err, os.ErrPermission) {
				reason = "unreadable"
			}
			// Parser errors can contain credentials from unrelated YAML values.
			log.Warn("cannot read outbound private-host permissions; private sources denied", "config_path", cfg.ConfigPath, "reason", reason)
		}
	}
	return serverconfig.WatchOutboundPrivateHosts(cfg.ConfigPath, cfg.OutboundRestrictPrivateHosts, cfg.OutboundAllowPrivateHosts, report)
}

var _ api.OutboundPolicySettings = (*serverSettings)(nil)

func (s *serverSettings) OutboundAllowPrivateHosts() []string {
	if s.readPrivateHosts != nil {
		return s.readPrivateHosts()
	}
	return privateHostPermissions(s.cfg, nil)()
}

func (a *aiService) privateHosts() []string {
	if a.readPrivateHosts != nil {
		return a.readPrivateHosts()
	}
	return a.outboundAllowPrivateHosts
}
