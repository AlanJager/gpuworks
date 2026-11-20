package monitorapi

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
)

// maxSignalPoints bounds the points of one series of a Signals answer: a
// window asked at a finer step than that is answered at a coarser one.
const maxSignalPoints = 600

// Signals serves GET /monitor/deployments/{id}/signals: the deployment's
// panel signals over [startMS, endMS] (monitor.BuildPanelSignals), the engine's
// own where the engine reports them and the gateway's where it does not. It
// is not cached: its window ends at the caller's now and slides with every
// poll, and it reads only the few series the panel draws.
func (s *Service) Signals(ctx context.Context, id uint, startMS, endMS int64, step time.Duration) (dto.MonitorDeploymentSignals, error) {
	inv, err := s.deps.Inventory(ctx)
	if err != nil {
		return dto.MonitorDeploymentSignals{}, err
	}
	if _, ok := findDeployment(inv.Deployments, id); !ok {
		return dto.MonitorDeploymentSignals{}, s.deps.NotFound
	}
	var instances []monitor.InstanceRef
	for _, inst := range inv.Instances {
		if inst.DeploymentID == id {
			instances = append(instances, inst)
		}
	}
	stepMS := step.Milliseconds()
	if floor := (endMS - startMS + maxSignalPoints - 1) / maxSignalPoints; stepMS < floor {
		stepMS = floor
	}
	if stepMS < 1000 {
		stepMS = 1000
	}
	signals, err := monitor.BuildPanelSignals(s.deps.TSDB, id, instances, startMS, endMS, stepMS)
	if err != nil {
		return dto.MonitorDeploymentSignals{}, err
	}
	return dto.MonitorDeploymentSignals{
		StartMS: startMS, EndMS: endMS, StepMS: stepMS,
		Sources:     sourcesDTO(signals.Sources),
		Concurrency: chartSeriesDTO(signals.Charts["concurrency"], 1),
		KV:          chartSeriesDTO(signals.Charts["kv"], 100),
		Throughput:  chartSeriesDTO(signals.Charts["throughput"], 1),
	}, nil
}
