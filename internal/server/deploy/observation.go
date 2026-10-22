package deploy

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// EngineMetricMap is the stored-series view of an engine's built-in mapping
// (inferencemetrics.EngineRules): exposition family to gpuworks_infer_* name.
// It is what Profile.MetricMap shows on the engines API.
func EngineMetricMap(engine string) map[string]string {
	rules := inferencemetrics.EngineRules(engine)
	if rules == nil {
		return nil
	}
	out := make(map[string]string, len(rules))
	for _, rule := range rules {
		if definition, ok := inferencemetrics.Lookup(rule.Metric); ok {
			out[rule.Source] = definition.Series()
		}
	}
	return out
}

// Observation is the resolved observation of an instance (ADR 0039) and the
// legacy fields an agent that predates it reads instead.
type Observation struct {
	Instance     proto.InstanceObservation
	LegacyPath   string
	LegacyPrefix string
	LegacyMap    map[string]string
}

// Apply stamps the observation on a desired state. It is the only writer of
// the four fields, at launch and on an in-place refresh alike.
func (o Observation) Apply(spec *proto.InstanceDesiredState) {
	instance := o.Instance
	spec.Observation = &instance
	spec.MetricPath, spec.MetricPrefix, spec.MetricMap = o.LegacyPath, o.LegacyPrefix, o.LegacyMap
}

// ResolveObservation decides how an instance of engine is observed, given the
// api.metrics its template declares (nil when it declares none). It is the one
// place this is decided:
//
//   - the mapping starts from the inherited engine's built-in rules, or the
//     deployment's own engine's when nothing is inherited;
//   - the template's own entries are laid over it, winning on the same family;
//   - an engine with no rules at all (callable) that declares nothing is
//     matched by family prefix against every built-in mapping, so a vLLM or
//     SGLang derivative needs no declaration.
func ResolveObservation(engine string, declared *inferenceapi.Metrics) Observation {
	if engine == "" {
		engine = DefaultEngine
	}
	profile := profileOf(engine)
	path := profile.MetricPath
	if path == "" {
		path = "/metrics"
	}
	base := inferencemetrics.EngineRules(engine)
	var own []inferencemetrics.Rule
	if declared != nil {
		if endpoint := strings.TrimSpace(declared.Endpoint); endpoint != "" {
			path = endpoint
		}
		if inherit := strings.TrimSpace(declared.Inherit); inherit != "" {
			base = inferencemetrics.EngineRules(inherit)
		}
		own = declared.Rules()
	}
	observation := Observation{LegacyPath: path, LegacyPrefix: profile.MetricPrefix}
	observation.Instance.MetricPath = path
	if declared != nil && declared.Format == string(inferencemetrics.FormatJSON) {
		// A JSON endpoint has only the paths the template declares: no
		// engine mapping applies and nothing is detected. An older agent
		// reads the endpoint as Prometheus text and finds nothing in it,
		// so it gets no legacy map.
		observation.Instance.MetricFormat = string(inferencemetrics.FormatJSON)
		observation.Instance.MetricRules = wireRules(inferencemetrics.Overlay(nil, own))
		return observation
	}
	rules := inferencemetrics.Overlay(base, own)
	if len(rules) == 0 {
		// Nothing declared and nothing known: detect by prefix. An older
		// agent keeps its legacy prefix fallback, which is what it did before.
		for _, set := range inferencemetrics.DetectSets() {
			observation.Instance.MetricDetect = append(observation.Instance.MetricDetect, proto.MetricRuleSet{
				Prefix: set.Prefix, Rules: wireRules(set.Rules),
			})
		}
		return observation
	}
	observation.Instance.MetricRules = wireRules(rules)
	observation.LegacyMap = make(map[string]string, len(rules))
	for _, rule := range observation.Instance.MetricRules {
		// The legacy map cannot scale; an older agent skips a scaled family
		// rather than storing it in the wrong unit.
		if rule.Scale == 0 || rule.Scale == 1 {
			observation.LegacyMap[rule.Source] = rule.Series
		}
	}
	return observation
}

func wireRules(rules []inferencemetrics.Rule) []proto.MetricRule {
	out := make([]proto.MetricRule, 0, len(rules))
	for _, rule := range rules {
		definition, ok := inferencemetrics.Lookup(rule.Metric)
		if !ok {
			// Unreachable for a validated template; a rule naming no platform
			// metric could only store a series nothing reads.
			continue
		}
		scale := rule.Scale
		if scale == 1 {
			scale = 0
		}
		out = append(out, proto.MetricRule{Source: rule.Source, Series: definition.Series(), Scale: scale})
	}
	return out
}

func profileOf(engine string) Profile {
	selected, err := SelectEngine(engine)
	if err != nil {
		return Profile{}
	}
	return selected.Profile()
}
