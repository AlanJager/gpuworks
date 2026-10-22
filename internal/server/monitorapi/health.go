package monitorapi

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/healthcheck"
)

// Health is a deployment's health check on the wire: enabled and
// thresholdSec are the deployment's own settings, the rest what the checker
// found. A deployment with the check off reads off whatever the checker
// still holds, and one just turned on reads pending until its first round.
func (s *Service) Health(id uint, enabled bool, thresholdSec int) dto.DeploymentHealth {
	out := dto.DeploymentHealth{Status: string(healthcheck.StatusOff), ThresholdSec: thresholdSec, Recent: []dto.HealthProbe{}}
	if !enabled {
		return out
	}
	var h healthcheck.Health
	if s.deps.Health != nil {
		h = s.deps.Health(id)
	}
	out.Status = string(h.Status)
	if h.Status == healthcheck.StatusOff || h.Status == "" {
		out.Status = string(healthcheck.StatusPending)
	}
	for _, p := range h.Recent {
		out.Recent = append(out.Recent, dto.HealthProbe{
			AtMS: p.AtMS, Status: string(p.Status), LatencyMS: p.LatencyMS,
			HTTPStatus: p.HTTPStatus, InstanceUUID: p.InstanceUUID, Message: p.Message,
		})
	}
	if n := len(out.Recent); n > 0 {
		last := out.Recent[n-1]
		out.Last = &last
	}
	if ms, n := h.MeanOK(); n > 0 {
		out.RecentMeanMS, out.RecentOK = &ms, n
	}
	return out
}
