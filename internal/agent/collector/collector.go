// Package collector gathers GPU/host/inference metrics on a node and batches them to
// the server every 10s (plan §10). No external agent: vendor prober + /proc +
// scraping each running instance's metrics endpoint.
package collector

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/dgpucache"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/hostsim"
	"gpuworks.ai/gpuworks/internal/agent/sysinfo"
	"gpuworks.ai/gpuworks/internal/agent/traffic"
	"gpuworks.ai/gpuworks/internal/proto"
)

// dcgmSampler is the collector's view of an optional DCGM probe: exactly one
// command per Sample call, returning per-GPU-index fractions. *gpu.DCGMProbe
// implements it; tests inject a fake to count calls without touching PATH.
type dcgmSampler interface {
	Sample(ctx context.Context) (map[int]gpu.DCGMSample, error)
}

// RunningInstance identifies a live instance to scrape.
type RunningInstance struct {
	UUID         string
	APIPort      int
	Engine       string
	MetricPath   string
	MetricPrefix string
	MetricMap    map[string]string
	// Observation, when the server resolved one, replaces MetricPath and the
	// legacy map (prometheus.go mappingFor).
	Observation *proto.InstanceObservation
	GPUIndexes  []int
	VRAMLimit   int64
	DGPUCache   string
	// DeploymentID is the owning deployment (0 for a legacy/standalone
	// instance with none). When non-zero it is added as a "deployment" label
	// on every sample this instance produces, so the server-side monitor can
	// group replicas of one deployment without a per-sample store lookup.
	DeploymentID uint
}

// Collector periodically collects and emits metrics.
type Collector struct {
	nodeUUID string
	log      *slog.Logger
	prober   gpu.Prober
	// instances returns the currently-running instances (for metrics scrape + labels).
	instances func() []RunningInstance
	// emit sends a metric batch (routed to the server over ctrl).
	emit func(proto.MetricBatch)

	// modelsRoot is the local models directory; host disk I/O sampling
	// resolves the block device backing it. Empty (the default) simply skips
	// the disk series, same as an agent too old to set it.
	modelsRoot string
	hostSample *hostSampler
	roles      roleSampler

	// dcgm is nil unless the agent enabled and detected DCGM (plan §4.1); a
	// nil dcgm costs zero extra commands and produces zero extra series.
	dcgm         dcgmSampler
	dcgmWarnOnce sync.Once

	// gaugeDupWarnOnce holds a *sync.Once per (instance, metric) that has
	// shown more than one label combination for a gauge in a scrape (see
	// parsePrometheus). Keyed on the collector rather than per-scrape state
	// so a gauge that keeps splitting on every scrape logs once for the
	// collector's lifetime, not once per 10s tick.
	gaugeDupWarnOnce sync.Map

	// jsonCounters keeps the counters of JSON endpoints from dipping (see
	// counterHold).
	jsonCounters counterHold

	// host is the machine a simulated node stands for; nil on a real node.
	host *hostsim.Host
}

// New builds a Collector.
func New(nodeUUID string, prober gpu.Prober, instances func() []RunningInstance, emit func(proto.MetricBatch), log *slog.Logger) *Collector {
	if prober == nil {
		prober = gpu.New()
	}
	return &Collector{nodeUUID: nodeUUID, log: log, prober: prober, instances: instances, emit: emit, hostSample: newHostSampler(), roles: roleSampler{totals: traffic.Totals}}
}

// SetModelsRoot tells the collector which local path models live under, so
// host disk I/O sampling can resolve the block device backing it. Optional:
// called once by the assembly root after New, before the first tick.
func (c *Collector) SetModelsRoot(path string) { c.modelsRoot = path }

// SetSimulatedHost makes the collector report the machine a simulated node
// stands for (hostsim.Host); nil, the default, is the real host.
func (c *Collector) SetSimulatedHost(h *hostsim.Host) { c.host = h }

// SetDCGMProbe attaches an optional DCGM probe built by the assembly root
// (nil when disabled or dcgmi is not on PATH). A concrete *gpu.DCGMProbe
// parameter — rather than the dcgmSampler interface — means a nil probe here
// always clears c.dcgm to a true nil interface, never a non-nil interface
// wrapping a nil pointer (the classic Go typed-nil trap).
func (c *Collector) SetDCGMProbe(p *gpu.DCGMProbe) {
	if p == nil {
		c.dcgm = nil
		return
	}
	c.dcgm = p
}

// Run collects every 10s (a simulated node at its own interval) until ctx
// is cancelled.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.host.CollectInterval(10 * time.Second))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.RunOnce(ctx)
		}
	}
}

// RunOnce collects and emits one batch. Production uses Run's fixed 10-second
// cadence; exposing the one-shot operation keeps lifecycle tests fast and
// deterministic without weakening that contract.
func (c *Collector) RunOnce(ctx context.Context) {
	batch := c.collect(ctx)
	if len(batch.Samples) > 0 {
		c.emit(batch)
	}
}

func (c *Collector) collect(ctx context.Context) proto.MetricBatch {
	now := time.Now().UnixMilli()
	var samples []proto.MetricSample
	samples = append(samples, c.gpuSamples(ctx, now)...)
	samples = append(samples, c.dgpuSamples(now)...)
	samples = append(samples, c.hostSamples(now)...)
	samples = append(samples, c.roles.sample(c.nodeUUID, time.Now(), now)...)
	samples = append(samples, c.inferSamples(ctx, now)...)
	return proto.MetricBatch{Samples: samples}
}

func (c *Collector) dgpuSamples(now int64) []proto.MetricSample {
	var samples []proto.MetricSample
	for _, in := range c.instances() {
		if in.VRAMLimit <= 0 || in.DGPUCache == "" || len(in.GPUIndexes) == 0 {
			continue
		}
		snapshot, err := dgpucache.ReadCache(in.DGPUCache, len(in.GPUIndexes))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, dgpucache.ErrCacheUninitialized) {
				c.log.Debug("dGPU cache read failed", "instance", in.UUID, "err", err)
			}
			continue
		}
		for visibleIndex, usage := range snapshot.Devices {
			labels := map[string]string{
				"node": c.nodeUUID, "gpu": strconv.Itoa(in.GPUIndexes[visibleIndex]), "instance": in.UUID,
			}
			AddDeploymentLabel(labels, in.DeploymentID)
			samples = append(samples,
				proto.MetricSample{Metric: "gpuworks_dgpu_memory_used_bytes", Labels: labels, Value: float64(usage.MemoryUsedBytes), TimeMS: now},
				proto.MetricSample{Metric: "gpuworks_dgpu_sm_util", Labels: labels, Value: float64(usage.SMUtilPercent), TimeMS: now},
			)
		}
	}
	return samples
}

// AddDeploymentLabel sets labels["deployment"] to id, unless id is 0 (a
// legacy/standalone instance with no owning deployment): metrics from such an
// instance keep exactly the label set they had before deployment labeling
// existed. Exported so other agent-side producers of deployment-scoped
// samples (engine's KV disk sweeper) format the label the same way instead of
// duplicating the strconv call.
func AddDeploymentLabel(labels map[string]string, id uint) {
	if id == 0 {
		return
	}
	labels["deployment"] = strconv.FormatUint(uint64(id), 10)
}

// gpuSamples reads per-GPU utilization/memory/temp/power via the selected vendor
// prober and emits stable gpuworks_gpu_* metric names.
func (c *Collector) gpuSamples(ctx context.Context, now int64) []proto.MetricSample {
	if c.prober == nil {
		return nil
	}
	// A slow driver may answer from an earlier query; the samples then carry
	// that query's time, not this tick's.
	ctx, answered := gpu.WithResultTime(ctx)
	metrics, err := c.prober.Metrics(ctx)
	if err != nil {
		c.log.Debug("gpu metrics failed", "vendor", c.prober.Vendor(), "err", err)
		return nil
	}
	if at := answered.Oldest(); !at.IsZero() {
		now = at.UnixMilli()
	}
	// DCGM's profiling counters (SM_ACTIVE/SM_OCCUPANCY/TENSOR_ACTIVE) exist
	// only on NVIDIA; a domestic-vendor node with c.dcgm nil never reaches
	// dcgmRunCommand at all, so this guard is belt-and-suspenders.
	if c.dcgm != nil && strings.EqualFold(c.prober.Vendor(), accelerator.VendorNVIDIA) {
		if samples, dErr := c.dcgm.Sample(ctx); dErr != nil {
			// Logged once per collector lifetime, not once per 10s tick: a node
			// that lost dcgmi (or never had permissions for it) must not spam
			// the log forever while it keeps reporting the rest of its metrics.
			c.dcgmWarnOnce.Do(func() { c.log.Warn("dcgm probe failed", "err", dErr) })
		} else {
			mergeDCGMMetrics(metrics, samples)
		}
	}
	return gpuMetricSamples(metrics, c.nodeUUID, now)
}

// mergeDCGMMetrics folds DCGM's per-GPU-index fractions into the same Metric
// slice nvidia-smi produced, keyed by the index both probes report, so the
// gpuworks_gpu_sm_* series carry the exact same node/gpu/vendor/gpu_uuid
// labels as every other per-GPU sample instead of a second identity lookup.
func mergeDCGMMetrics(metrics []gpu.Metric, samples map[int]gpu.DCGMSample) {
	for i := range metrics {
		s, ok := samples[metrics[i].Index]
		if !ok {
			continue
		}
		metrics[i].Values["gpuworks_gpu_sm_active"] = s.SMActive
		metrics[i].Values["gpuworks_gpu_sm_occupancy"] = s.SMOccupancy
		metrics[i].Values["gpuworks_gpu_tensor_active"] = s.TensorActive
	}
}

func gpuMetricSamples(metrics []gpu.Metric, nodeUUID string, now int64) []proto.MetricSample {
	var samples []proto.MetricSample
	for _, m := range metrics {
		if len(m.Values) == 0 {
			continue
		}
		lbl := map[string]string{"node": nodeUUID, "gpu": strconv.Itoa(m.Index), "vendor": m.Vendor}
		if m.UUID != "" {
			lbl["gpu_uuid"] = m.UUID
		}
		names := make([]string, 0, len(m.Values))
		for name := range m.Values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			samples = append(samples, proto.MetricSample{Metric: name, Labels: lbl, Value: m.Values[name], TimeMS: now})
		}
	}
	return samples
}

// hostSamples reports node CPU/memory/disk/network usage. Memory is an
// instantaneous gauge; CPU/disk/network are rates that need two ticks, so the
// first call after a Collector is created emits none of those three.
func (c *Collector) hostSamples(now int64) []proto.MetricSample {
	sys := c.host.SysInfo(sysinfo.Collect())
	lbl := map[string]string{"node": c.nodeUUID}
	used := (sys.MemTotalKB - sys.MemAvailKB) * 1024
	samples := []proto.MetricSample{
		{Metric: "gpuworks_node_mem_used_bytes", Labels: lbl, Value: float64(used), TimeMS: now},
		{Metric: "gpuworks_node_mem_total_bytes", Labels: lbl, Value: float64(sys.MemTotalKB * 1024), TimeMS: now},
	}
	if sim, ok := c.host.HostSamples(c.nodeUUID, now); ok {
		samples = append(samples, sim...)
	} else if c.hostSample != nil {
		samples = append(samples, c.hostSample.sample(c.nodeUUID, c.modelsRoot, now)...)
	}
	return samples
}

// inferSamples scrapes each running instance's metrics endpoint and normalizes
// selected engine metrics to the gpuworks_infer_* namespace.
func (c *Collector) inferSamples(ctx context.Context, now int64) []proto.MetricSample {
	var samples []proto.MetricSample
	client := &http.Client{Timeout: 3 * time.Second}
	instances := c.instances()
	running := make(map[string]struct{}, len(instances))
	for _, in := range instances {
		running[in.UUID] = struct{}{}
		path := in.MetricPath
		if in.Observation != nil && in.Observation.MetricPath != "" {
			path = in.Observation.MetricPath
		}
		if path == "" {
			path = "/metrics"
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(in.APIPort)+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if in.Observation != nil && in.Observation.MetricFormat == "json" {
			samples = append(samples, parseJSONMetrics(resp.Body, in, c.nodeUUID, now, c.log, &c.jsonCounters)...)
		} else {
			samples = append(samples, parsePrometheus(resp.Body, in, c.nodeUUID, now, c.log, c.warnGaugeDuplicateOnce)...)
		}
		resp.Body.Close()
	}
	c.jsonCounters.retain(running)
	return samples
}

// warnGaugeDuplicateOnce logs, at most once per (instance, metric) for this
// collector's lifetime, that a gauge exposed more than one label combination
// in a scrape (see parsePrometheus). Called on every occurrence, including
// repeats on later scrapes; the sync.Once per key is what keeps that down to
// one actual log line instead of one per 10s tick.
func (c *Collector) warnGaugeDuplicateOnce(instance, metric string) {
	key := instance + "\x00" + metric
	onceIface, _ := c.gaugeDupWarnOnce.LoadOrStore(key, &sync.Once{})
	onceIface.(*sync.Once).Do(func() {
		c.log.Warn("gauge metric had multiple label combinations in one scrape; keeping the first value", "instance", instance, "metric", metric)
	})
}
