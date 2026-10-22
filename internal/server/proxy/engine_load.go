package proxy

import (
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type engineGauge struct {
	waiting, kv float64
	at          time.Time
}
type engineLoad struct {
	mu     sync.RWMutex
	byInst map[string]engineGauge
	now    func() time.Time
}

func newEngineLoad() *engineLoad {
	return &engineLoad{byInst: make(map[string]engineGauge), now: time.Now}
}

func (l *engineLoad) observe(samples []proto.MetricSample) {
	if l == nil {
		return
	}
	nowFn := l.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, old := range l.byInst {
		if !old.at.IsZero() && now.Sub(old.at) > engineGaugeMaxAge {
			delete(l.byInst, key)
		}
	}
	for _, s := range samples {
		name := s.Metric
		if name != "gpuworks_infer_requests_waiting" && name != "gpuworks_infer_gpu_cache_usage_percent" {
			continue
		}
		inst := s.Labels["instance"]
		if inst == "" {
			continue
		}
		v := s.Value
		if name == "gpuworks_infer_gpu_cache_usage_percent" && v > 1.5 {
			v /= 100
		}
		g := l.byInst[inst]
		if name == "gpuworks_infer_requests_waiting" {
			g.waiting = v
		} else {
			g.kv = v
		}
		g.at = now
		l.byInst[inst] = g
	}
}

func (l *engineLoad) gauge(instance string) (engineGauge, bool) {
	if l == nil {
		return engineGauge{}, false
	}
	l.mu.RLock()
	g, ok := l.byInst[instance]
	l.mu.RUnlock()
	now := l.now
	if now == nil {
		now = time.Now
	}
	if !ok || now().Sub(g.at) > engineGaugeMaxAge {
		return engineGauge{}, false
	}
	return g, true
}
