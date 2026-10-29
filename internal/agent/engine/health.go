package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// probeAndReport polls the health endpoint until ready or the budget expires,
// then reports running/failed. It returns true only once running was reported.
func (e *Engine) probeAndReport(ctx context.Context, spec proto.InstanceDesiredState, id string) bool {
	if spec.Headless {
		return e.probeHeadlessAndReport(ctx, spec, id)
	}
	driver := e.Driver()
	timeout := time.Duration(spec.StartTimeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	healthPath := spec.HealthPath
	if healthPath == "" {
		healthPath = "/health"
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", spec.APIPort, healthPath)
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	healthStarted := time.Now()
	nextHealthReport := 10 * time.Second
	loadRequested := false
	var loadResult <-chan string

	for time.Now().Before(deadline) {
		st, err := driver.Status(ctx, id)
		if ctx.Err() != nil {
			return false
		}
		if err == nil && !st.Exists {
			e.failAfterContainer(ctx, spec, "container disappeared during startup")
			return false
		}
		if err == nil && st.Exists && !st.Running {
			msg := "container OOM-killed"
			if !e.detectOOM(ctx, id, st) {
				msg = e.exitMessage(ctx, id, fmt.Sprintf("container exited (code %d)", st.ExitCode))
			}
			e.failAfterContainer(ctx, spec, msg)
			return false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			e.failAfterContainer(ctx, spec, "invalid engine health endpoint: "+err.Error())
			return false
		}
		resp, err := client.Do(req)
		if err == nil {
			readiness, decodeErr := readEngineReadiness(resp)
			ready := resp.StatusCode == http.StatusOK
			if ready && readiness.Service == "strata" {
				ready = decodeErr == nil && readiness.Loaded != nil && *readiness.Loaded
				if decodeErr == nil && !ready && readiness.Loaded != nil && readiness.APIKey != nil && !*readiness.APIKey {
					modelsReq, modelsErr := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/models", spec.APIPort), nil)
					var modelsResp *http.Response
					if modelsErr == nil {
						modelsResp, modelsErr = client.Do(modelsReq)
					}
					if modelsErr == nil {
						models, valid := readStrataModels(modelsResp)
						if valid {
							ready = intentionallyUnloaded(models)
							if len(models) == 0 && !loadRequested {
								loadRequested = true
								e.reportStateProgress(spec, proto.InstStarting, id, "", "engine child is not loaded; recovering", proto.PhaseHealthCheck, 0, 0, 0, 0)
								loadResult = requestStrataLoad(ctx, client, spec.APIPort)
							}
						}
					}
				}
			}
			if ready {
				// A healthy answer on the port may come from someone else's
				// engine while ours is still loading weights.
				if err := checkServesOwnModel(ctx, client, spec); err != nil {
					e.failAfterContainer(ctx, spec, err.Error())
					return false
				}
				e.log.Info("instance healthy", "uuid", spec.UUID, "port", spec.APIPort)
				e.mu.Lock()
				doPrewarm := spec.Sleepable && spec.DesiredSleep == 0 && !e.prewarmed[spec.UUID]
				if doPrewarm {
					e.prewarmed[spec.UUID] = true
				}
				e.mu.Unlock()
				if doPrewarm {
					e.reportState(spec, proto.InstOptimizing, id, "", "prewarming sleep")
					if err := e.prewarmSleep(ctx, spec); err != nil {
						e.failAfterContainer(ctx, spec, "sleep prewarm failed: "+err.Error())
						return false
					}
				}
				e.mu.Lock()
				actualSleep := e.sleepLevel[spec.UUID]
				e.mu.Unlock()
				e.report(proto.InstanceStatus{
					UUID: spec.UUID, State: proto.InstRunning, ContainerID: id,
					Ports: spec.Ports, DirectAddr: fmt.Sprintf("127.0.0.1:%d", spec.APIPort),
					Measured: e.collectMeasured(ctx, spec, id), SleepLevel: actualSleep,
					OpenAPIPaths: readOpenAPIPaths(ctx, client, spec.APIPort),
				})
				return true
			}
		}
		if loadResult != nil {
			select {
			case detail := <-loadResult:
				loadResult = nil
				if detail != "" && ctx.Err() == nil {
					e.reportStateProgress(spec, proto.InstStarting, id, "", detail, proto.PhaseHealthCheck, 0, 0, 0, 0)
				}
			default:
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
			// The message stays identical for every heartbeat: a progress
			// report is overwritable state, and an elapsed counter baked into
			// the text would make each one look like a new milestone. The UI
			// derives "how long" from the event and instance timestamps.
			if elapsed := time.Since(healthStarted); elapsed >= nextHealthReport {
				e.reportStateProgress(spec, proto.InstStarting, id, "", "waiting for engine health", proto.PhaseHealthCheck, 0, 0, 0, 0)
				nextHealthReport += 10 * time.Second
			}
		}
	}
	e.failAfterContainer(ctx, spec, "health check timed out")
	return false
}

// probeHeadlessAndReport verifies that a distributed worker remains alive for
// several consecutive runtime inspections. A follower intentionally has no
// HTTP API, so probing APIPort would always time out. The leader's HTTP health
// check plus the server-side all-ranks readiness gate complete the group check.
func (e *Engine) probeHeadlessAndReport(ctx context.Context, spec proto.InstanceDesiredState, id string) bool {
	driver := e.Driver()
	timeout := time.Duration(spec.StartTimeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	interval := e.headlessProbeInterval
	if interval <= 0 {
		interval = time.Second
	}
	required := e.headlessStableChecks
	if required <= 0 {
		required = 3
	}
	deadline := time.Now().Add(timeout)
	stable := 0
	for time.Now().Before(deadline) {
		st, err := driver.Status(ctx, id)
		if ctx.Err() != nil {
			return false
		}
		if err == nil && !st.Exists {
			e.failAfterContainer(ctx, spec, "headless worker disappeared during startup")
			return false
		}
		if err == nil && st.Exists {
			if !st.Running || st.Health == "unhealthy" {
				var msg string
				switch {
				case e.detectOOM(ctx, id, st):
					msg = "headless worker OOM-killed"
				case st.Running && st.Health == "unhealthy":
					msg = "headless worker is unhealthy"
				default:
					msg = e.exitMessage(ctx, id, fmt.Sprintf("headless worker exited (code %d)", st.ExitCode))
				}
				e.failAfterContainer(ctx, spec, msg)
				return false
			}
			stable++
			if stable >= required {
				e.log.Info("headless worker stable", "uuid", spec.UUID, "rank", spec.NodeRank)
				e.report(proto.InstanceStatus{
					UUID: spec.UUID, State: proto.InstRunning, ContainerID: id,
					Message: fmt.Sprintf("headless rank %d ready", spec.NodeRank),
				})
				return true
			}
		} else {
			stable = 0
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
	e.failAfterContainer(ctx, spec, "headless worker readiness timed out")
	return false
}

func (e *Engine) detectOOM(ctx context.Context, id string, st runtime.Status) bool {
	if st.OOMKilled {
		return true
	}
	if st.ExitCode != 137 {
		return false
	}
	rc, err := e.Driver().Logs(ctx, id, false, 200)
	if err != nil {
		return false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 256*1024))
	return err == nil && (strings.Contains(string(b), "OutOfMemoryError") || strings.Contains(string(b), "CUDA out of memory"))
}

// exitMessage says why a container exited: a cause the output names in words
// (enginelog.HumanizeStartupFailure), else base with what stopped it: its
// dGPU memory limit when the interposer refused an allocation, else the last
// error line the engine printed - or "with no output" when it printed
// nothing, which a full disk also produces. Removal deletes the output, so this is what is left of
// it on the failed instance.
func (e *Engine) exitMessage(ctx context.Context, id, base string) string {
	rc, err := e.Driver().Logs(ctx, id, false, 200)
	if err != nil {
		return base
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 256*1024))
	if err != nil {
		return base
	}
	text := string(data)
	if human := enginelog.HumanizeStartupFailure(text); human != "" {
		return human
	}
	if limit := enginelog.DGPULimitExceeded(text); limit != "" {
		return base + ": " + limit
	}
	if last := enginelog.LastError(text); last != "" {
		return base + ": " + last
	}
	if strings.TrimSpace(text) == "" {
		return base + " with no output"
	}
	return base
}

// collectMeasured reads recent container logs and parses the engine's real
// memory breakdown for estimate calibration. Unsupported log formats are
// intentionally silent.
func (e *Engine) collectMeasured(ctx context.Context, spec proto.InstanceDesiredState, id string) json.RawMessage {
	// The window must reach back to the start of the launch: the SGLang parser
	// reports no memory sums from a tail that has lost it, and an engine on
	// many GPUs logs dozens of lines per GPU before it is healthy. This runs
	// once per healthy transition, so the larger read is not on a hot path.
	rc, err := e.Driver().Logs(ctx, id, false, 4000)
	if err != nil {
		return nil
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 4<<20))
	if err != nil {
		return nil
	}
	m := enginelog.ParseDialect(spec.LogDialect, string(data))
	if !m.Found {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}
