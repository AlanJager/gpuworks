package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// DataRetention is how long the platform keeps each kind of observation and
// statistic, and the time zone days are counted in. It is one settings row
// (SettingDataRetention), read whole by every component that retains; the
// settings page edits it, and every value takes effect without a restart.
type DataRetention struct {
	// MetricsHours keeps metrics.db's raw samples, LogsHours logs.db's lines.
	MetricsHours int `json:"metrics_hours"`
	LogsHours    int `json:"logs_hours"`
	// RequestLogDays and RequestLogMaxMB bound ux.db: whole days are dropped
	// past the age and, oldest first, past the size (never today).
	RequestLogDays  int `json:"request_log_days"`
	RequestLogMaxMB int `json:"request_log_max_mb"`
	// The token usage, kept on its own because customers are billed by it.
	UsageHourlyDays int `json:"usage_hourly_days"`
	UsageDailyDays  int `json:"usage_daily_days"`
	// Service quality, GPU utilization and card time share one retention;
	// each can be switched off (it stops accumulating, what it has ages out).
	StatsHourlyDays int  `json:"stats_hourly_days"`
	StatsDailyDays  int  `json:"stats_daily_days"`
	Quality         bool `json:"quality"`
	GPU             bool `json:"gpu"`
	GPUTime         bool `json:"gpu_time"`
	// TimeZone is the IANA zone days are rolled up in; a change applies to
	// days rolled up after it.
	TimeZone string `json:"time_zone"`
}

// DataRetentionLimit is the range a retention value may take.
type DataRetentionLimit struct{ Min, Max int }

// DataRetentionLimits are the bounds of every numeric value, by JSON name;
// the settings page reads them from the API rather than knowing them.
var DataRetentionLimits = map[string]DataRetentionLimit{
	"metrics_hours":      {24, 30 * 24},
	"logs_hours":         {24, 30 * 24},
	"request_log_days":   {1, 90},
	"request_log_max_mb": {512, 50 * 1024},
	"usage_hourly_days":  {7, 93},
	"usage_daily_days":   {31, 1825},
	"stats_hourly_days":  {7, 93},
	"stats_daily_days":   {31, 1825},
}

// DefaultDataRetention is what a new installation keeps: metricsRetention for
// samples and lines (the metrics_retention option, 72h unless configured),
// 7 days or 2 GiB of requests, 31 days of hours and 400 of days.
func DefaultDataRetention(metricsRetention time.Duration, zone string) DataRetention {
	hours := int(metricsRetention / time.Hour)
	limit := DataRetentionLimits["metrics_hours"]
	hours = min(max(hours, limit.Min), limit.Max)
	return DataRetention{
		MetricsHours: hours, LogsHours: hours,
		RequestLogDays: 7, RequestLogMaxMB: 2048,
		UsageHourlyDays: 31, UsageDailyDays: 400,
		StatsHourlyDays: 31, StatsDailyDays: 400,
		Quality: true, GPU: true, GPUTime: true,
		TimeZone: zone,
	}
}

// DataRetentionError names the value a DataRetention got wrong.
type DataRetentionError struct {
	Field  string
	Detail string
}

func (e *DataRetentionError) Error() string { return e.Field + ": " + e.Detail }

// Validate checks every value against its bounds and the zone against the
// time zone database.
func (r DataRetention) Validate() error {
	for _, v := range []struct {
		name  string
		value int
	}{
		{"metrics_hours", r.MetricsHours}, {"logs_hours", r.LogsHours},
		{"request_log_days", r.RequestLogDays}, {"request_log_max_mb", r.RequestLogMaxMB},
		{"usage_hourly_days", r.UsageHourlyDays}, {"usage_daily_days", r.UsageDailyDays},
		{"stats_hourly_days", r.StatsHourlyDays}, {"stats_daily_days", r.StatsDailyDays},
	} {
		limit := DataRetentionLimits[v.name]
		if v.value < limit.Min || v.value > limit.Max {
			return &DataRetentionError{Field: v.name, Detail: fmt.Sprintf("must be between %d and %d", limit.Min, limit.Max)}
		}
	}
	if r.UsageDailyDays < r.UsageHourlyDays {
		return &DataRetentionError{Field: "usage_daily_days", Detail: "days must be kept at least as long as hours"}
	}
	if r.StatsDailyDays < r.StatsHourlyDays {
		return &DataRetentionError{Field: "stats_daily_days", Detail: "days must be kept at least as long as hours"}
	}
	if _, err := r.Location(); err != nil {
		return &DataRetentionError{Field: "time_zone", Detail: "not a time zone: " + r.TimeZone}
	}
	return nil
}

// Location is the statistics time zone.
func (r DataRetention) Location() (*time.Location, error) {
	if r.TimeZone == "" {
		return nil, fmt.Errorf("no time zone")
	}
	return time.LoadLocation(r.TimeZone)
}

// DataRetention reads the setting, or fallback when it is missing or broken.
func (s *Store) DataRetention(fallback DataRetention) DataRetention {
	raw := s.GetSetting(SettingDataRetention)
	if raw == "" {
		return fallback
	}
	var out DataRetention
	if json.Unmarshal([]byte(raw), &out) != nil || out.Validate() != nil {
		return fallback
	}
	return out
}

// SetDataRetention validates and stores the setting.
func (s *Store) SetDataRetention(r DataRetention) error {
	if err := r.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.SetSetting(SettingDataRetention, string(b))
}

// EnsureDataRetention stores initial, the configured defaults, unless the
// setting exists; it returns what is in effect. A setting a newer edit made
// wins over the configuration from then on.
func (s *Store) EnsureDataRetention(initial DataRetention) (DataRetention, bool, error) {
	if raw := s.GetSetting(SettingDataRetention); raw != "" {
		return s.DataRetention(initial), false, nil
	}
	if err := s.SetDataRetention(initial); err != nil {
		return initial, false, err
	}
	return initial, true, nil
}
