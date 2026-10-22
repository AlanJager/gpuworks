package monitorapi

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/platformmetrics"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// Metrics is the catalog of every stored series the platform defines - the
// inference vocabulary and the platform's own GPU, host, network, KV disk,
// benchmark and gateway series - for the metric explorer and the assistant
// to list instead of spelling names themselves. It is static: both
// vocabularies are compiled in.
func (s *Service) Metrics() []dto.MonitorMetricSeries {
	return metricCatalog()
}

func metricCatalog() []dto.MonitorMetricSeries {
	out := metricVocabularyDTO()
	for _, d := range platformmetrics.Definitions() {
		out = append(out, dto.MonitorMetricSeries{
			Series: d.Series, Name: strings.TrimPrefix(d.Series, "gpuworks_"), Source: string(d.Source), Kind: string(d.Kind),
			Unit: d.Unit, Labels: d.Labels, Description: d.Description,
		})
	}
	return out
}

func metricVocabularyDTO() []dto.MonitorMetricSeries {
	out := []dto.MonitorMetricSeries{}
	add := func(source, series string, definition inferencemetrics.Definition) {
		info := dto.MonitorMetricSeries{Series: series, Name: definition.Name, Source: source, Kind: string(definition.Kind), Unit: definition.Unit}
		if definition.Kind != inferencemetrics.Histogram {
			out = append(out, info)
			return
		}
		quantiles := info
		quantiles.Quantiles = true
		sum, count := info, info
		sum.Series, sum.Kind = series+"_sum", string(inferencemetrics.Counter)
		count.Series, count.Kind, count.Unit = series+"_count", string(inferencemetrics.Counter), "requests"
		out = append(out, quantiles, sum, count)
	}
	for _, definition := range inferencemetrics.Definitions() {
		add("engine", definition.Series(), definition)
	}
	for _, definition := range inferencemetrics.GatewayDefinitions() {
		add("gateway", definition.Series(), definition.Definition)
	}
	return out
}
