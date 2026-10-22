package collector

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/proto"
)

// parseJSONMetrics reads an engine endpoint that answers one JSON document
// (InstanceObservation.MetricFormat "json", Strata's /metrics): each rule's
// source is the dotted key path of a number in it. A path that is absent or
// not a number (or a boolean, read as 1/0) yields no sample; a document over
// oneMiB, or one that does not decode, yields none at all - a cut JSON
// document has no trustworthy part.
// Counters pass through hold (nil holds nothing).
func parseJSONMetrics(r io.Reader, in RunningInstance, node string, now int64, log *slog.Logger, hold *counterHold) []proto.MetricSample {
	body, err := io.ReadAll(io.LimitReader(r, oneMiB+1))
	if err != nil {
		return nil
	}
	if len(body) > oneMiB {
		if log != nil {
			log.Warn("json metrics document over 1 MiB; not read", "instance", in.UUID)
		}
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document any
	if decoder.Decode(&document) != nil {
		return nil
	}

	values := map[string]float64{}
	var order []string
	for _, rule := range in.Observation.MetricRules {
		source, ok := inferencemetrics.ParseJSONSource(rule.Source)
		if !ok {
			continue
		}
		value, ok := jsonValue(document, source)
		if !ok {
			continue
		}
		if rule.Scale != 0 {
			value *= rule.Scale
		}
		if _, seen := values[rule.Series]; !seen {
			order = append(order, rule.Series)
		}
		// Two paths mapped to one series add up, as duplicate counter lines
		// of a Prometheus scrape do.
		values[rule.Series] += value
	}
	for series, value := range values {
		if definition, ok := inferencemetrics.Lookup(strings.TrimPrefix(series, inferencemetrics.SeriesPrefix)); ok && definition.Kind == inferencemetrics.Counter {
			values[series] = hold.apply(in.UUID, series, value)
		}
	}
	derived := inferencemetrics.Derive(values)
	derivedNames := make([]string, 0, len(derived))
	for metric := range derived {
		derivedNames = append(derivedNames, metric)
	}
	sort.Strings(derivedNames)
	for _, metric := range derivedNames {
		values[metric] = derived[metric]
		order = append(order, metric)
	}

	samples := make([]proto.MetricSample, 0, len(order))
	for _, metric := range order {
		samples = append(samples, proto.MetricSample{Metric: metric, Labels: instanceLabels(in, node), Value: values[metric], TimeMS: now})
	}
	return samples
}

// jsonValue walks source's keys through nested objects to a number or a
// boolean, or, for a comparison, to a string it tests.
func jsonValue(document any, source inferencemetrics.JSONSource) (float64, bool) {
	current := document
	for _, key := range source.Keys {
		object, ok := current.(map[string]any)
		if !ok {
			return 0, false
		}
		if current, ok = object[key]; !ok {
			return 0, false
		}
	}
	if source.Compare != "" {
		text, ok := current.(string)
		if !ok {
			return 0, false
		}
		if (text == source.Value) == (source.Compare == "==") {
			return 1, true
		}
		return 0, true
	}
	switch value := current.(type) {
	case json.Number:
		f, err := value.Float64()
		return f, err == nil
	case bool:
		if value {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// counterHold keeps a JSON endpoint's counters from dipping. A template may
// sum a counter from a finished total and an in-flight part - Strata's
// totals.output_tokens + live.generated - so throughput shows while a request
// runs, not all at once when it ends. That sum falls back by the in-flight
// part when a request is cancelled before it reaches the total, and the
// monitor reads any fall as a counter reset: the whole new reading would count
// as one interval's increase, a spike of everything generated so far. A fall
// that keeps at least half of the last reading is held at that reading until
// the counter climbs past it again; a deeper fall is a real reset (the engine
// restarted) and passes through, as a Prometheus counter's would.
type counterHold struct {
	mu sync.Mutex
	// last is the reading passed on per instance and series.
	last map[counterKey]float64
}

type counterKey struct{ instance, series string }

func (h *counterHold) apply(instance, series string, value float64) float64 {
	if h == nil {
		return value
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := counterKey{instance, series}
	if last, ok := h.last[key]; ok && value < last && value >= last/2 {
		return last
	}
	if h.last == nil {
		h.last = map[counterKey]float64{}
	}
	h.last[key] = value
	return value
}

// retain forgets the counters of instances no longer running.
func (h *counterHold) retain(running map[string]struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key := range h.last {
		if _, ok := running[key.instance]; !ok {
			delete(h.last, key)
		}
	}
}

// instanceLabels are the labels every sample of an instance carries.
func instanceLabels(in RunningInstance, node string) map[string]string {
	engine := in.Engine
	if engine == "" {
		engine = engines.VLLM
	}
	labels := map[string]string{"node": node, "instance": in.UUID, "engine": engine}
	AddDeploymentLabel(labels, in.DeploymentID)
	return labels
}
