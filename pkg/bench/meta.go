// Package bench contains the shared product-level benchmark dictionary. Engine
// subpackages own execution algorithms; this package owns values presented by
// both the API and UI so their defaults cannot drift.
package bench

import (
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/localization"
)

const (
	DefaultInputTokens                = 4096
	DefaultOutputTokens               = 1024
	DefaultRequestsPerPointMultiplier = 10
	DefaultMaxConcurrency             = 32
	DefaultMinThroughputGain          = 0.05
	DefaultDetailCapMB                = 20
	MinDetailCapMB                    = 1
	MaxDetailCapMB                    = 50

	// Open-loop (arrival-rate) load. Burstiness is the gamma shape of
	// inter-arrival times; 1 is a Poisson process.
	DefaultBurstiness           = 1.0
	DefaultPointDurationSeconds = 60.0
	MinPointDurationSeconds     = 5.0
	MaxPointDurationSeconds     = 3600.0
	MinOpenLoopRequests         = 20
	MaxRequestRate              = 10000.0
	MaxRequestRates             = 32
	MaxBurstiness               = 100.0
	MaxInFlight                 = 100000
	// Warmup requests run once before the first point and are discarded. New
	// runs default to one: it absorbs the first-request cold start at almost
	// no cost.
	DefaultWarmupRequests = 1
	MaxWarmupRequests     = 1000
	// Each request, warmup included, is abandoned after its timeout. Ten
	// minutes covers a long-context prefill queued behind other requests;
	// a run that sends longer requests raises it.
	DefaultRequestTimeoutSeconds = 600
	MinRequestTimeoutSeconds     = 10
	MaxRequestTimeoutSeconds     = 7200
)

type Option struct {
	Value       string            `json:"value"`
	Label       localization.Text `json:"label"`
	Description localization.Text `json:"description,omitempty"`
	Color       string            `json:"color,omitempty"`
}

type NumberField struct {
	Key       string            `json:"key"`
	Label     localization.Text `json:"label"`
	Unit      string            `json:"unit,omitempty"`
	Direction string            `json:"direction"`
	Min       float64           `json:"min"`
	Max       float64           `json:"max"`
	Step      float64           `json:"step"`
}

type ModeDefaults struct {
	Concurrency                any     `json:"concurrency"`
	MaxConcurrency             int     `json:"max_concurrency,omitempty"`
	ISL                        []int   `json:"isl"`
	OSL                        []int   `json:"osl"`
	RequestsPerPointMultiplier int     `json:"requests_per_point_multiplier"`
	MinThroughputGain          float64 `json:"min_throughput_gain,omitempty"`
	Objective                  string  `json:"objective,omitempty"`
}

type DetailCapMeta struct {
	Default int    `json:"default"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
	Unit    string `json:"unit"`
}

type AgenticDefaultsMeta struct {
	Preset                    string         `json:"preset"`
	Turns                     int            `json:"turns"`
	SubsequentTurnInputTokens int            `json:"subsequent_turn_input_tokens"`
	OutputTokens              int            `json:"output_tokens"`
	Seed                      int64          `json:"seed"`
	Concurrency               []int          `json:"concurrency"`
	SessionsPerPoint          map[string]int `json:"sessions_per_point"`
	SessionsPerConcurrency    int            `json:"sessions_per_concurrency"`
	MinSessionsPerPoint       int            `json:"min_sessions_per_point"`
}

type Meta struct {
	Modes            []Option                   `json:"modes"`
	ModeDefaults     map[string]ModeDefaults    `json:"mode_defaults"`
	Datasets         []Option                   `json:"datasets"`
	DatasetProfiles  map[string]dataset.Profile `json:"dataset_profiles"`
	SLAConstraints   []NumberField              `json:"sla_constraints"`
	Objectives       []Option                   `json:"objectives"`
	Statuses         []Option                   `json:"statuses"`
	TokenizerSchemes map[string]string          `json:"tokenizer_schemes"`
	DetailCapMB      DetailCapMeta              `json:"detail_cap_mb"`
	AgenticPresets   []dataset.AgenticPreset    `json:"agentic_presets"`
	AgenticDefaults  AgenticDefaultsMeta        `json:"agentic_defaults"`
	RetiredDatasets  []Option                   `json:"retired_datasets,omitempty"`
	LoadDefaults     LoadDefaultsMeta           `json:"load_defaults"`
}

// LoadDefaultsMeta holds the defaults and bounds of the advanced load
// settings: warmup, open-loop arrivals, the per-request timeout and the
// fixed-output-length switch.
type LoadDefaultsMeta struct {
	WarmupRequests       int     `json:"warmup_requests"`
	MaxWarmupRequests    int     `json:"max_warmup_requests"`
	Burstiness           float64 `json:"burstiness"`
	PointDurationSeconds float64 `json:"point_duration_s"`
	MinPointDuration     float64 `json:"min_point_duration_s"`
	MaxPointDuration     float64 `json:"max_point_duration_s"`
	MinOpenLoopRequests  int     `json:"min_open_loop_requests"`
	MaxRequestRate       float64 `json:"max_request_rate"`
	MaxRequestRates      int     `json:"max_request_rates"`
	MaxBurstiness        float64 `json:"max_burstiness"`
	MaxInFlight          int     `json:"max_in_flight"`
	RequestTimeout       int     `json:"request_timeout_s"`
	MinRequestTimeout    int     `json:"min_request_timeout_s"`
	MaxRequestTimeout    int     `json:"max_request_timeout_s"`
	// IgnoreEOS is the default of the fixed-output-length switch for the
	// datasets that offer it (IgnoreEOSDatasets).
	IgnoreEOS         bool     `json:"ignore_eos"`
	IgnoreEOSDatasets []string `json:"ignore_eos_datasets"`
}

func Dictionary() Meta {
	return localizeMeta(dictionaryDefinition())
}

func dictionaryDefinition() Meta {
	profiles, _ := dataset.BuiltinProfiles()
	return Meta{
		Modes: []Option{
			{Value: "saturation"},
			{Value: "sla"},
			{Value: "grid"},
		},
		ModeDefaults: map[string]ModeDefaults{
			"saturation": {Concurrency: 1, MaxConcurrency: DefaultMaxConcurrency, ISL: []int{DefaultInputTokens}, OSL: []int{DefaultOutputTokens}, RequestsPerPointMultiplier: DefaultRequestsPerPointMultiplier, MinThroughputGain: DefaultMinThroughputGain},
			"sla":        {Concurrency: 1, MaxConcurrency: DefaultMaxConcurrency, ISL: []int{DefaultInputTokens}, OSL: []int{DefaultOutputTokens}, RequestsPerPointMultiplier: DefaultRequestsPerPointMultiplier, Objective: "rps"},
			"grid":       {Concurrency: []int{1, 2, 4}, ISL: []int{DefaultInputTokens}, OSL: []int{DefaultOutputTokens}, RequestsPerPointMultiplier: DefaultRequestsPerPointMultiplier},
		},
		Datasets: []Option{
			{Value: "random"},
			{Value: "agentic"},
			{Value: "multiturn"},
			{Value: "imported"},
		},
		RetiredDatasets: []Option{{Value: "agenttrace"}},
		AgenticPresets:  dataset.AgenticPresets(),
		AgenticDefaults: AgenticDefaultsMeta{Preset: dataset.AgenticDefaultPreset, Turns: dataset.AgenticDefaultTurns, SubsequentTurnInputTokens: dataset.AgenticSubsequentTurnInputTokens, OutputTokens: dataset.AgenticOutputTokens, Seed: dataset.AgenticDefaultSeed, Concurrency: []int{1, 2, 4, 8}, SessionsPerPoint: map[string]int{"1": 4, "2": 8, "4": 8, "8": 16}, SessionsPerConcurrency: 2, MinSessionsPerPoint: 4},
		DatasetProfiles: profiles,
		SLAConstraints: []NumberField{
			{Key: "max_avg_ttft", Unit: "ms", Direction: "max", Min: 0, Max: 3600000, Step: 1},
			{Key: "max_p99_ttft", Unit: "ms", Direction: "max", Min: 0, Max: 3600000, Step: 1},
			{Key: "max_avg_tpot", Unit: "ms", Direction: "max", Min: 0, Max: 3600000, Step: 1},
			{Key: "max_p99_tpot", Unit: "ms", Direction: "max", Min: 0, Max: 3600000, Step: 1},
			{Key: "max_avg_latency", Unit: "ms", Direction: "max", Min: 0, Max: 3600000, Step: 1},
			{Key: "max_p99_latency", Unit: "ms", Direction: "max", Min: 0, Max: 3600000, Step: 1},
			{Key: "max_rps", Unit: "req/s", Direction: "max", Min: 0, Max: 1000000, Step: .1},
			{Key: "max_tps", Unit: "tok/s", Direction: "max", Min: 0, Max: 1000000000, Step: 1},
			{Key: "min_rps", Unit: "req/s", Direction: "min", Min: 0, Max: 1000000, Step: .1},
			{Key: "min_tps", Unit: "tok/s", Direction: "min", Min: 0, Max: 1000000000, Step: 1},
			{Key: "min_interactivity", Unit: "tok/s/user", Direction: "min", Min: 0, Max: 100000, Step: .1},
			{Key: "min_session_completion", Direction: "min", Min: 0, Max: 1, Step: .01},
		},
		Objectives:       []Option{{Value: "rps"}, {Value: "output_tps"}, {Value: "total_tps"}},
		Statuses:         []Option{{Value: "pending", Color: "gold"}, {Value: "running", Color: "blue"}, {Value: "done", Color: "green"}, {Value: "failed", Color: "red"}, {Value: "canceled", Color: "default"}},
		TokenizerSchemes: map[string]string{"huggingface": "hf", "modelscope": "ms", "upload": "file"},
		DetailCapMB:      DetailCapMeta{Default: DefaultDetailCapMB, Min: MinDetailCapMB, Max: MaxDetailCapMB, Unit: "MB"},
		LoadDefaults: LoadDefaultsMeta{
			WarmupRequests: DefaultWarmupRequests, MaxWarmupRequests: MaxWarmupRequests,
			Burstiness: DefaultBurstiness, PointDurationSeconds: DefaultPointDurationSeconds,
			MinPointDuration: MinPointDurationSeconds, MaxPointDuration: MaxPointDurationSeconds,
			MinOpenLoopRequests: MinOpenLoopRequests, MaxRequestRate: MaxRequestRate, MaxRequestRates: MaxRequestRates,
			MaxBurstiness: MaxBurstiness, MaxInFlight: MaxInFlight,
			RequestTimeout: DefaultRequestTimeoutSeconds, MinRequestTimeout: MinRequestTimeoutSeconds, MaxRequestTimeout: MaxRequestTimeoutSeconds,
			IgnoreEOS: true, IgnoreEOSDatasets: dataset.IgnoreEOSKinds(),
		},
	}
}
