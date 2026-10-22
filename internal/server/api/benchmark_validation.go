package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"gpuworks.ai/gpuworks/internal/benchrun"
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/search"
)

func benchmarkParameterError(field, reason string) error {
	return productError(ErrorBenchmarkParameterInvalid, reason, map[string]any{"field": field})
}

// Admission validates client input before endpoint probes or conflict checks.
// Stored documents remain backward compatible in the execution readers.
func decodeBenchmarkConfig(raw json.RawMessage, dst any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return fmt.Errorf("benchmark configuration must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

func validateBenchmarkSearch(mode string, raw json.RawMessage, dc dataset.Config) (search.Document, error) {
	var doc search.Document
	if err := decodeBenchmarkConfig(raw, &doc); err != nil {
		return doc, benchmarkParameterError("search", err.Error())
	}
	switch search.Mode(mode) {
	case "", search.Grid, search.Saturation, search.SLA:
	default:
		return doc, benchmarkParameterError("mode", fmt.Sprintf("unknown benchmark mode %q", mode))
	}
	positive := func(field string, values []int) error {
		for _, value := range values {
			if value <= 0 {
				return benchmarkParameterError("search."+field, field+" values must be positive")
			}
		}
		return nil
	}
	conc, err := doc.Concurrencies()
	if err != nil {
		return doc, benchmarkParameterError("search.concurrency", "invalid concurrency: "+err.Error())
	}
	if len(doc.Concurrency) > 0 && len(conc) == 0 {
		return doc, benchmarkParameterError("search.concurrency", "concurrency must be a positive integer or a non-empty list of positive integers")
	}
	for _, field := range []struct {
		name   string
		values []int
	}{{"concurrency", conc}, {"fixed_concurrency", doc.FixedConcurrency}, {"isl", doc.ISL}, {"osl", doc.OSL}} {
		if err := positive(field.name, field.values); err != nil {
			return doc, err
		}
	}
	if doc.MaxConcurrency < 0 {
		return doc, benchmarkParameterError("search.max_concurrency", "max_concurrency cannot be negative")
	}
	adaptive := mode == string(search.Saturation) || mode == string(search.SLA)
	if adaptive && len(doc.FixedConcurrency) == 0 && doc.MaxConcurrency > 0 && len(conc) > 0 && conc[0] > doc.MaxConcurrency {
		return doc, benchmarkParameterError("search.max_concurrency", "starting concurrency cannot exceed max_concurrency")
	}

	for _, field := range []struct {
		name  string
		value float64
	}{
		{"min_throughput_gain", doc.MinThroughputGain}, {"requests_per_point_multiplier", doc.RequestsPerPointMultiplier},
		{"max_avg_ttft", doc.MaxAvgTTFT}, {"max_p99_ttft", doc.MaxP99TTFT}, {"max_avg_tpot", doc.MaxAvgTPOT}, {"max_p99_tpot", doc.MaxP99TPOT},
		{"max_avg_latency", doc.MaxAvgLatency}, {"max_p99_latency", doc.MaxP99Latency}, {"max_rps", doc.MaxRPS}, {"max_tps", doc.MaxTPS}, {"min_rps", doc.MinRPS}, {"min_tps", doc.MinTPS}, {"min_interactivity", doc.MinInteractivity},
		{"max_per_user_total_tps", doc.LegacyMaxPerUserTotalTPS}, {"min_per_user_total_tps", doc.LegacyMinPerUserTotalTPS}, {"max_per_user_output_tps", doc.LegacyMaxPerUserOutputTPS}, {"min_per_user_output_tps", doc.LegacyMinPerUserOutputTPS},
	} {
		if field.value < 0 {
			return doc, benchmarkParameterError("search."+field.name, field.name+" cannot be negative")
		}
	}
	if (doc.MaxRPS > 0 && doc.MinRPS > doc.MaxRPS) || (doc.MaxTPS > 0 && doc.MinTPS > doc.MaxTPS) {
		return doc, benchmarkParameterError("search", "minimum throughput cannot exceed maximum throughput")
	}
	for key, count := range doc.SessionsPerPoint {
		concurrency, err := strconv.Atoi(key)
		if err != nil || concurrency <= 0 || strconv.Itoa(concurrency) != key || count <= 0 {
			return doc, benchmarkParameterError("search.sessions_per_point", "sessions_per_point requires positive concurrency keys and positive session counts")
		}
	}
	switch doc.Objective {
	case "", "rps", "request_tps", "tps", "output_tps", "output_tok_s", "total_tps", "total_tok_s":
	default:
		return doc, benchmarkParameterError("search.objective", fmt.Sprintf("unknown benchmark objective %q", doc.Objective))
	}
	if err := validateBenchmarkRequestCounts(mode, doc, conc, dc); err != nil {
		return doc, err
	}
	return doc, nil
}

// Validate the request counts of the points the selected runner can execute.
// Adaptive bisection may visit any integer between its first point and maximum.
func validateBenchmarkRequestCounts(mode string, doc search.Document, conc []int, dc dataset.Config) error {
	adaptive := mode == string(search.Saturation) || mode == string(search.SLA)
	if dc.Kind != "agentic" {
		dc.InputTokens = benchmeta.DefaultInputTokens
		if len(doc.ISL) > 0 {
			dc.InputTokens = doc.ISL[0]
		}
	}
	if err := benchrun.ValidateAllocation(dc, 1); err != nil {
		return benchmarkParameterError("search.isl", err.Error())
	}
	// Warmup only uses the first point. A free adaptive search also uses only
	// its first ISL; grid and fixed-point strategies visit every length.
	if err := benchrun.ValidateAllocation(dc, max(0, doc.WarmupRequests)); err != nil {
		return benchmarkParameterError("search.warmup_requests", err.Error())
	}
	if dc.Kind != "agentic" && (!adaptive || len(doc.FixedConcurrency) > 0) {
		for _, tokens := range doc.ISL {
			dc.InputTokens = max(dc.InputTokens, tokens)
		}
		if err := benchrun.ValidateAllocation(dc, 1); err != nil {
			return benchmarkParameterError("search.isl", err.Error())
		}
	}
	check := func(point search.Point) error {
		field := "search.requests_per_point_multiplier"
		if point.RequestRate > 0 {
			field = "search.request_rates"
		} else if doc.SessionsPerPoint[strconv.Itoa(point.Concurrency)] > 0 {
			field = "search.sessions_per_point"
		}
		count, err := doc.RequestsForPoint(point)
		if err == nil {
			err = benchrun.ValidateAllocation(dc, count)
		}
		if err != nil {
			return benchmarkParameterError(field, err.Error())
		}
		return nil
	}
	if doc.OpenLoop() {
		for _, rate := range doc.RequestRates {
			if err := check(search.Point{RequestRate: rate}); err != nil {
				return err
			}
		}
		return nil
	}
	if adaptive && len(doc.FixedConcurrency) > 0 {
		conc = doc.FixedConcurrency
	} else {
		if len(conc) == 0 {
			conc = []int{1}
		}
		if adaptive {
			conc = conc[:1]
			if doc.MaxConcurrency > 0 {
				// Checking overrides plus the largest unoverridden point covers the
				// entire range without iterating a potentially enormous maximum.
				for key := range doc.SessionsPerPoint {
					c, _ := strconv.Atoi(key)
					if c >= conc[0] && c <= doc.MaxConcurrency {
						if err := check(search.Point{Concurrency: c}); err != nil {
							return err
						}
					}
				}
				for upper := doc.MaxConcurrency; upper >= conc[0]; upper-- {
					if doc.SessionsPerPoint[strconv.Itoa(upper)] == 0 {
						return check(search.Point{Concurrency: upper})
					}
				}
				return nil
			}
		}
	}
	for _, concurrency := range conc {
		if err := check(search.Point{Concurrency: concurrency}); err != nil {
			return err
		}
	}
	return nil
}

func validateBenchmarkDataset(raw json.RawMessage) (dataset.Config, error) {
	var config dataset.Config
	if err := decodeBenchmarkConfig(raw, &config); err != nil {
		return config, err
	}
	switch config.Kind {
	case "", "random", "multiturn", "imported", "agentic", "agenttrace":
	default:
		return config, fmt.Errorf("unknown dataset kind %q", config.Kind)
	}
	for _, field := range []struct {
		name  string
		value int
	}{{"input_tokens", config.InputTokens}, {"output_tokens", config.OutputTokens}, {"turns", config.Turns}, {"mean_input_tokens", config.MeanInputTokens}} {
		if field.value < 0 {
			return config, fmt.Errorf("dataset.%s cannot be negative", field.name)
		}
	}
	if config.Kind == "agentic" {
		return dataset.ExpandAgenticConfig(config)
	}
	if config.Kind == "imported" {
		if len(config.Samples) == 0 {
			return config, fmt.Errorf("imported dataset has no samples")
		}
		for i, sample := range config.Samples {
			if len(sample.Messages) == 0 {
				return config, fmt.Errorf("dataset.samples[%d] has no messages", i)
			}
			for _, messages := range [][]dataset.Message{sample.Messages, sample.FollowUps} {
				for _, message := range messages {
					if message.Role == "" || message.Content == "" {
						return config, fmt.Errorf("dataset.samples[%d] message requires role and content", i)
					}
				}
			}
			if sample.InputTokens < 0 || sample.OutputTokens < 0 || sample.WaitMillis < 0 {
				return config, fmt.Errorf("dataset.samples[%d] lengths and wait_millis cannot be negative", i)
			}
		}
	}
	return config, nil
}
