// Prometheus text-exposition parsing for scraped instances: turning one
// /metrics response into gpuworks_infer_* samples, including histogram
// family expansion and same-scrape duplicate-label-combo collapsing (see
// parsePrometheus's own doc comment for why that collapsing exists).
package collector

import (
	"bufio"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/proto"
)

// oneMiB bounds how much of one instance's /metrics response parsePrometheus
// will ever read. A histogram family alone can add dozens of _bucket lines
// per instance; without an overall cap a misbehaving or enormous exposition
// would grow collector memory unbounded instead of just losing its tail.
const oneMiB = 1 << 20

// countingReader remembers how many bytes it has delivered, so a reader
// wrapped in io.LimitReader(cr, oneMiB+1) can tell afterward whether the
// limit actually cut the stream short (n > oneMiB) as opposed to the body
// simply ending under the cap on its own.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// prometheusEntry accumulates one gpuworks-side (metric, extra-labels) tuple
// across every exposition line that maps to it within a single scrape.
type prometheusEntry struct {
	metric      string
	extraLabels map[string]string
	value       float64
	isCounter   bool
}

// parsePrometheus extracts curated gauges/counters/histograms from Prometheus
// exposition.
//
// A single gpuworks metric can be fed by several exposition lines in one
// scrape — most commonly a counter family broken out by a label the gpuworks
// side does not carry (e.g. vllm:request_success_total{finished_reason=...}
// split into stop/length/abort rows that all map to the same bare
// gpuworks_infer_requests_success_total). Emitting one sample per line would
// leave tsdb with whichever row happened to be scanned last — for a counter
// that is a value that swings between the real total and a single row's
// count on every scrape. So every line is first folded into at most one
// (metric, extra-labels) entry per scrape: counters (the exposition name ends
// in "_total", or it is a histogram _bucket/_sum/_count line) are summed;
// gauges keep the first value seen. warnGaugeDup (nil-safe) is called every
// time a gauge shows more than one label combination in a scrape; the caller
// (Collector) decides how often that actually becomes a log line — see
// Collector.warnGaugeDuplicateOnce, which logs once per (instance, metric)
// for the collector's lifetime rather than once per scrape.
func parsePrometheus(r io.Reader, in RunningInstance, node string, now int64, log *slog.Logger, warnGaugeDup func(instance, metric string)) []proto.MetricSample {
	lines := readExposition(r, in.UUID, log)
	metricMap := mappingFor(in, lines)
	baseLabels := func() map[string]string { return instanceLabels(in, node) }

	entries := make(map[string]*prometheusEntry, 16)
	order := make([]string, 0, 16)

	add := func(dst string, extra map[string]string, val float64, isCounter bool) {
		key := dedupKey(dst, extra)
		if existing, ok := entries[key]; ok {
			if isCounter {
				existing.value += val
				return
			}
			// Gauge duplicate: the first value already stored wins. How often
			// this becomes an actual log line is the caller's call, not
			// this function's — a raw parsePrometheus (as agent tests call
			// it) has no per-collector state to dedupe against.
			if warnGaugeDup != nil {
				warnGaugeDup(in.UUID, dst)
			}
			return
		}
		entries[key] = &prometheusEntry{metric: dst, extraLabels: extra, value: val, isCounter: isCounter}
		order = append(order, key)
	}

	for _, line := range lines {
		if dst, extra, ok := lookupHistogram(metricMap, line.name, line.labels); ok {
			add(dst, extra, line.value, true)
			continue
		}
		target, ok := lookupMetric(metricMap, line.name, line.labels)
		if !ok {
			continue
		}
		add(target.series, nil, line.value*target.factor(), strings.HasSuffix(line.name, "_total"))
	}

	// Metrics an engine reports only through others (a KV usage ratio from
	// pool size and free space) are derived from this scrape's own values.
	unlabeled := make(map[string]float64, len(order))
	for _, key := range order {
		if e := entries[key]; len(e.extraLabels) == 0 {
			unlabeled[e.metric] = e.value
		}
	}
	derived := inferencemetrics.Derive(unlabeled)
	derivedNames := make([]string, 0, len(derived))
	for metric := range derived {
		derivedNames = append(derivedNames, metric)
	}
	sort.Strings(derivedNames)
	for _, metric := range derivedNames {
		add(metric, nil, derived[metric], false)
	}

	samples := make([]proto.MetricSample, 0, len(order))
	for _, key := range order {
		e := entries[key]
		lbl := baseLabels()
		for k, v := range e.extraLabels {
			lbl[k] = v
		}
		samples = append(samples, proto.MetricSample{Metric: e.metric, Labels: lbl, Value: e.value, TimeMS: now})
	}
	return samples
}

// expositionLine is one sample line of a scrape, taken apart.
type expositionLine struct {
	name   string
	labels string
	value  float64
}

// readExposition reads one /metrics response into its sample lines, at most
// oneMiB of it. When the body ran past the cap, the line being assembled right
// at the boundary may be a partial line whose truncated text still parses as a
// (wrong) number, so that last line is dropped - but only if it was the last
// line scanned: a skipped comment or malformed line at the boundary must not
// cost the complete sample before it.
func readExposition(r io.Reader, instance string, log *slog.Logger) []expositionLine {
	// The +1 lets the reader tell whether the real stream had more than
	// oneMiB bytes (truncating it) as opposed to legitimately ending at or
	// under the cap.
	counting := &countingReader{r: io.LimitReader(r, oneMiB+1)}
	sc := bufio.NewScanner(counting)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var lines []expositionLine
	lastScannedParsed := false
	for sc.Scan() {
		lastScannedParsed = false
		text := sc.Text()
		if text == "" || text[0] == '#' {
			continue
		}
		name, labels, value, ok := parseMetricLine(text)
		if !ok {
			continue
		}
		lines = append(lines, expositionLine{name: name, labels: labels, value: value})
		lastScannedParsed = true
	}
	if err := sc.Err(); err != nil && log != nil {
		log.Warn("metrics scrape scan error", "instance", instance, "err", err)
	}
	if counting.n > oneMiB && lastScannedParsed {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// metricTarget is where one exposition family goes: a stored series and the
// factor its value is multiplied by.
type metricTarget struct {
	series string
	scale  float64
}

func (t metricTarget) factor() float64 {
	if t.scale == 0 {
		return 1
	}
	return t.scale
}

// mappingFor is the mapping one scrape is read with. An instance the server
// resolved an observation for uses it: its rules, or - when it states none -
// the first detect set whose family prefix the scrape actually shows. An
// instance from a server that predates observations keeps the legacy map.
func mappingFor(in RunningInstance, lines []expositionLine) map[string]metricTarget {
	if in.Observation == nil {
		legacy := normalizedMetricMap(in)
		out := make(map[string]metricTarget, len(legacy))
		for source, series := range legacy {
			out[source] = metricTarget{series: series}
		}
		return out
	}
	rules := in.Observation.MetricRules
	if len(rules) == 0 {
		rules = detectRules(in.Observation.MetricDetect, lines)
	}
	out := make(map[string]metricTarget, len(rules))
	for _, rule := range rules {
		out[rule.Source] = metricTarget{series: rule.Series, scale: rule.Scale}
	}
	return out
}

func detectRules(sets []proto.MetricRuleSet, lines []expositionLine) []proto.MetricRule {
	for _, set := range sets {
		prefix := set.Prefix + ":"
		for _, line := range lines {
			if strings.HasPrefix(line.name, prefix) {
				return set.Rules
			}
		}
	}
	return nil
}

// dedupKey identifies one gpuworks-side sample within a scrape: the metric
// name plus any extra labels the collector itself adds (today only a
// histogram bucket's "le"). Sorted so the key is stable regardless of map
// iteration order, even though only one extra label exists today.
func dedupKey(metric string, extra map[string]string) string {
	if len(extra) == 0 {
		return metric
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(metric)
	for _, k := range keys {
		b.WriteByte('|')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(extra[k])
	}
	return b.String()
}

// histogramSuffixes are the three exposition-side suffixes prometheus_client
// appends to every histogram family. A MetricMap entry names only the base
// family (no suffix); the collector expands it to all three gpuworks-side
// series as each suffixed line is seen, one line at a time.
var histogramSuffixes = [...]string{"_bucket", "_sum", "_count"}

// lookupHistogram resolves a histogram-family exposition line against the
// base family name in metricMap. Only a mapped base family is expanded; an
// exposition histogram nobody mapped produces nothing, exactly like an
// unmapped gauge or counter today. A _bucket line without a well-formed "le"
// label is dropped rather than guessed at.
func lookupHistogram(metricMap map[string]metricTarget, name, labels string) (dst string, extraLabels map[string]string, ok bool) {
	for _, suffix := range histogramSuffixes {
		base := strings.TrimSuffix(name, suffix)
		if base == name {
			continue // name does not end in this suffix
		}
		target, mapped := metricMap[base]
		if !mapped {
			continue
		}
		baseDst := target.series
		if suffix != "_bucket" {
			return baseDst + suffix, nil, true
		}
		le, hasLe := leLabel(labels)
		if !hasLe {
			return "", nil, false
		}
		return baseDst + suffix, map[string]string{"le": le}, true
	}
	return "", nil, false
}

// leLabel extracts the "le" (bucket upper bound) label a histogram _bucket
// line always carries, preserving it verbatim (including "+Inf") since the
// server, not the agent, does the quantile math.
func leLabel(labels string) (string, bool) {
	for _, have := range splitLabels(labels) {
		key, value, ok := strings.Cut(have, "=")
		if ok && key == "le" {
			return unquoteLabel(value), true
		}
	}
	return "", false
}

func normalizedMetricMap(in RunningInstance) map[string]string {
	if len(in.MetricMap) > 0 {
		out := make(map[string]string, len(in.MetricMap))
		for k, v := range in.MetricMap {
			out[k] = v
		}
		return out
	}
	prefix := in.MetricPrefix
	if prefix == "" {
		prefix = in.Engine
	}
	if prefix == "" {
		prefix = engines.VLLM
	}
	key := func(name string) string { return prefix + ":" + name }
	return map[string]string{
		key("num_requests_running"):    "gpuworks_infer_requests_running",
		key("num_requests_waiting"):    "gpuworks_infer_requests_waiting",
		key("gpu_cache_usage_perc"):    "gpuworks_infer_gpu_cache_usage_percent",
		key("generation_tokens_total"): "gpuworks_infer_generation_tokens_total",
		key("prompt_tokens_total"):     "gpuworks_infer_prompt_tokens_total",
	}
}

// parseMetricLine parses "name{labels} value" → (name, label text, value). The
// label text is returned verbatim, without the braces, because only a mapping
// that asks about a label pays the cost of taking it apart.
func parseMetricLine(line string) (string, string, float64, bool) {
	sp := strings.LastIndexByte(line, ' ')
	if sp < 0 {
		return "", "", 0, false
	}
	val, err := strconv.ParseFloat(strings.TrimSpace(line[sp+1:]), 64)
	if err != nil {
		return "", "", 0, false
	}
	name, labels := line[:sp], ""
	if b := strings.IndexByte(name, '{'); b >= 0 {
		labels = strings.TrimSuffix(strings.TrimSpace(name[b+1:]), "}")
		name = name[:b]
	}
	return strings.TrimSpace(name), labels, val, true
}

// lookupMetric resolves one exposition line against the instance's metric map.
// A map key may carry a label selector - `name{tier="1:fs"}` - which is how one
// engine metric reported once per tier becomes several gpuworks metrics, since
// the samples that reach the time series carry only node, instance and engine.
// Selectors are tried before the bare name, so an engine that reports the
// metric without labels still matches a bare mapping exactly as before.
func lookupMetric(metricMap map[string]metricTarget, name, labels string) (metricTarget, bool) {
	if strings.Contains(name, "{") {
		// A malformed line whose name still holds a brace cannot be matched
		// against a selector without guessing where the labels start.
		return metricTarget{}, false
	}
	if labels != "" {
		// A well-formed mapping has at most one selector per line, but map
		// iteration order must not decide the answer if one ever has two: the
		// lowest key wins, so the same exposition always yields the same sample.
		best, matched := "", metricTarget{}
		for key, dst := range metricMap {
			selector, ok := metricSelector(key, name)
			if !ok || !labelsSelect(labels, selector) {
				continue
			}
			if best == "" || key < best {
				best, matched = key, dst
			}
		}
		if best != "" {
			return matched, true
		}
	}
	dst, ok := metricMap[name]
	return dst, ok
}

// metricSelector returns the label selector a map key states for name, and
// whether the key is a selector for that name at all.
func metricSelector(key, name string) (string, bool) {
	if !strings.HasPrefix(key, name+"{") || !strings.HasSuffix(key, "}") {
		return "", false
	}
	return key[len(name)+1 : len(key)-1], true
}

// labelsSelect reports whether every label the selector states is present on
// the line with the same value. The selector is a subset test: a line may carry
// labels nobody asked about.
func labelsSelect(labels, selector string) bool {
	for _, want := range splitLabels(selector) {
		key, value, ok := strings.Cut(want, "=")
		if !ok {
			return false
		}
		found := false
		for _, have := range splitLabels(labels) {
			haveKey, haveValue, ok := strings.Cut(have, "=")
			if ok && haveKey == key && unquoteLabel(haveValue) == unquoteLabel(value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// splitLabels cuts a label list on commas. A value containing a comma or an
// escaped quote would be split wrongly, so such a value simply fails to match
// and its line produces no sample - the safe direction for a curated mapping.
func splitLabels(labels string) []string {
	parts := strings.Split(labels, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func unquoteLabel(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}
