package dto

// DataRetention is how long each kind of observation and statistic is kept,
// and the time zone statistics count days in (store.DataRetention).
type DataRetention struct {
	MetricsHours    int    `json:"metrics_hours"`
	LogsHours       int    `json:"logs_hours"`
	RequestLogDays  int    `json:"request_log_days"`
	RequestLogMaxMB int    `json:"request_log_max_mb"`
	UsageHourlyDays int    `json:"usage_hourly_days"`
	UsageDailyDays  int    `json:"usage_daily_days"`
	StatsHourlyDays int    `json:"stats_hourly_days"`
	StatsDailyDays  int    `json:"stats_daily_days"`
	Quality         bool   `json:"quality"`
	GPU             bool   `json:"gpu"`
	GPUTime         bool   `json:"gpu_time"`
	TimeZone        string `json:"time_zone"`
}

// DataRetentionLimit is the range a numeric value may take.
type DataRetentionLimit struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// DataRetentionView is the setting with what the settings page needs to edit
// it: every numeric value's bounds, by JSON name, and the server's own zone.
type DataRetentionView struct {
	Value          DataRetention                 `json:"value"`
	Limits         map[string]DataRetentionLimit `json:"limits"`
	ServerTimeZone string                        `json:"server_time_zone"`
}

// DataRetentionStore is what one kind of data holds now.
type DataRetentionStore struct {
	// Kind is metrics, logs, requests, usage, quality, gpu or gpu_time.
	Kind string `json:"kind"`
	// File is the database file the kind lives in (metrics.db, logs.db,
	// ux.db, stats.db, or the main database for usage).
	File string `json:"file"`
	// FileBytes is that file's size on disk; null for the main database,
	// which holds far more than the usage. Kinds sharing stats.db report the
	// same size.
	FileBytes *int64 `json:"file_bytes"`
	// HourlyRows and DailyRows count a statistic's rows; null for the raw
	// observations.
	HourlyRows *int64 `json:"hourly_rows"`
	DailyRows  *int64 `json:"daily_rows"`
	// EarliestMS is the oldest time the kind can still answer for (Unix ms):
	// what the request log holds, the retention edge of raw samples and
	// lines; null when there is nothing.
	EarliestMS *int64 `json:"earliest_ms"`
}

// DataRetentionPass is the last run of a background pass; zero AtMS when it
// has not run.
type DataRetentionPass struct {
	AtMS  int64  `json:"at_ms"`
	Error string `json:"error,omitempty"`
}

// DataRetentionStatus is the retention's effect: the stores, when live
// accumulation of the statistics began, how far the one-off fill from the
// request log got, and the background passes.
type DataRetentionStatus struct {
	Stores      []DataRetentionStore `json:"stores"`
	LiveSinceMS int64                `json:"live_since_ms"`
	Backfill    StatisticsBackfill   `json:"backfill"`
	Retain      DataRetentionPass    `json:"retain"`
	Rollup      DataRetentionPass    `json:"rollup"`
	// UsageFlush is the last token usage write: its rows and how long it
	// took, the cost that grows with the key x deployment combinations used.
	UsageFlush UsageFlush `json:"usage_flush"`
}

// StatisticsBackfill is how far back the fill reaches (Unix ms) per store.
type StatisticsBackfill struct {
	UsageDone        bool  `json:"usage_done"`
	UsageThroughMS   int64 `json:"usage_through_ms"`
	QualityDone      bool  `json:"quality_done"`
	QualityThroughMS int64 `json:"quality_through_ms"`
}

// UsageFlush describes the last token usage write.
type UsageFlush struct {
	AtMS  int64  `json:"at_ms"`
	Rows  int    `json:"rows"`
	MS    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}
