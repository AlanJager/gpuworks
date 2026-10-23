package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDataRetentionDefaultsValidationAndSeeding(t *testing.T) {
	def := DefaultDataRetention(72*time.Hour, "Asia/Shanghai")
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	if def.MetricsHours != 72 || def.LogsHours != 72 || def.RequestLogDays != 7 || def.RequestLogMaxMB != 2048 ||
		def.UsageHourlyDays != 31 || def.UsageDailyDays != 400 || !def.Quality || !def.GPU || !def.GPUTime {
		t.Fatalf("defaults = %+v", def)
	}
	// A configured retention outside the bounds is brought inside them.
	if long := DefaultDataRetention(90*24*time.Hour, "UTC"); long.MetricsHours != 30*24 {
		t.Fatalf("90d = %d", long.MetricsHours)
	}
	if short := DefaultDataRetention(time.Hour, "UTC"); short.MetricsHours != 24 {
		t.Fatalf("1h = %d", short.MetricsHours)
	}
	for _, tc := range []struct {
		field string
		mut   func(*DataRetention)
	}{
		{"metrics_hours", func(r *DataRetention) { r.MetricsHours = 23 }},
		{"logs_hours", func(r *DataRetention) { r.LogsHours = 721 }},
		{"request_log_days", func(r *DataRetention) { r.RequestLogDays = 0 }},
		{"request_log_max_mb", func(r *DataRetention) { r.RequestLogMaxMB = 100 }},
		{"usage_hourly_days", func(r *DataRetention) { r.UsageHourlyDays = 94 }},
		{"usage_daily_days", func(r *DataRetention) { r.UsageDailyDays = 1826 }},
		{"usage_daily_days", func(r *DataRetention) { r.UsageHourlyDays, r.UsageDailyDays = 90, 60 }},
		{"stats_daily_days", func(r *DataRetention) { r.StatsHourlyDays, r.StatsDailyDays = 90, 60 }},
		{"time_zone", func(r *DataRetention) { r.TimeZone = "Mars/Olympus" }},
		{"time_zone", func(r *DataRetention) { r.TimeZone = "" }},
	} {
		r := def
		tc.mut(&r)
		var fe *DataRetentionError
		if err := r.Validate(); !errors.As(err, &fe) || fe.Field != tc.field {
			t.Errorf("%s: err = %v", tc.field, err)
		}
	}

	st := newTestStore(t)
	got, created, err := st.EnsureDataRetention(def)
	if err != nil || !created || got != def {
		t.Fatalf("seed = %+v %v %v", got, created, err)
	}
	edited := def
	edited.UsageDailyDays, edited.GPU, edited.TimeZone = 730, false, "UTC"
	if err := st.SetDataRetention(edited); err != nil {
		t.Fatal(err)
	}
	// The configuration no longer wins over an edit.
	got, created, err = st.EnsureDataRetention(DefaultDataRetention(240*time.Hour, "Asia/Shanghai"))
	if err != nil || created || got != edited {
		t.Fatalf("after edit = %+v %v %v", got, created, err)
	}
	bad := edited
	bad.MetricsHours = 1
	if err := st.SetDataRetention(bad); err == nil {
		t.Fatal("an invalid value was stored")
	}
	if st.DataRetention(def) != edited {
		t.Fatal("a refused write changed the setting")
	}
	// The stored form is the JSON names the API speaks.
	var raw map[string]any
	if err := json.Unmarshal([]byte(st.GetSetting(SettingDataRetention)), &raw); err != nil || raw["usage_daily_days"] != float64(730) || raw["gpu"] != false || raw["time_zone"] != "UTC" {
		t.Fatalf("stored = %v %v", raw, err)
	}
	// A broken row reads as the fallback.
	if err := st.SetSetting(SettingDataRetention, `{"metrics_hours":1}`); err != nil {
		t.Fatal(err)
	}
	if st.DataRetention(def) != def {
		t.Fatal("a broken row was used")
	}
}
