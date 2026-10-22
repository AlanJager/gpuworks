package dto

// TelemetryExport is GET /telemetry/export: where the gateway's request
// traces go (store.TelemetryExport) and what the export did since the server
// started. Header values are secrets and never come back; a header is listed
// by name only.
type TelemetryExport struct {
	Enabled bool `json:"enabled"`
	// Endpoint is the collector's OTLP/HTTP address as entered.
	Endpoint string                `json:"endpoint"`
	Headers  []TelemetryHeaderName `json:"headers"`
	// SampleRatio is the share of the traces the gateway starts that are
	// exported, 0..1; a request in a caller's sampled trace is exported
	// whatever it is.
	SampleRatio float64 `json:"sample_ratio"`
	// TraceLink is a URL with {trace_id} that opens a trace in the tracing
	// backend; empty for none.
	TraceLink string                `json:"trace_link"`
	Status    TelemetryExportStatus `json:"status"`
}

// TelemetryHeaderName is a stored header, its value withheld.
type TelemetryHeaderName struct {
	Name string `json:"name"`
}

// TelemetryExportStatus is the running export.
type TelemetryExportStatus struct {
	// Active says spans are being sent; TracesURL is where.
	Active    bool   `json:"active"`
	TracesURL string `json:"traces_url"`
	// Exported counts spans the collector accepted since the server
	// started; Dropped those lost before sending (the queue was full);
	// Failed those the collector refused or that could not be delivered.
	Exported uint64 `json:"exported"`
	Dropped  uint64 `json:"dropped"`
	Failed   uint64 `json:"failed"`
	Queued   int    `json:"queued"`
	// LastError is the latest failure, with its time (Unix milliseconds);
	// LastExportAt the latest accepted batch. Null when none yet.
	LastError    string `json:"last_error"`
	LastErrorAt  *int64 `json:"last_error_at"`
	LastExportAt *int64 `json:"last_export_at"`
	// SettingError says why a stored setting could not be applied (it does
	// not decrypt); empty when it applies.
	SettingError string `json:"setting_error"`
}

// TelemetryExportUpdate is the body of PUT /telemetry/export and POST
// /telemetry/export/test: the whole setting. A header given without a value
// keeps the value stored under its name; a stored header left out is
// removed. SampleRatio absent is 1.
type TelemetryExportUpdate struct {
	Enabled     bool                    `json:"enabled"`
	Endpoint    string                  `json:"endpoint"`
	Headers     []TelemetryHeaderUpdate `json:"headers"`
	SampleRatio *float64                `json:"sample_ratio"`
	TraceLink   string                  `json:"trace_link"`
}

// TelemetryHeaderUpdate is one header of an update; a null value keeps the
// stored one.
type TelemetryHeaderUpdate struct {
	Name  string  `json:"name"`
	Value *string `json:"value"`
}

// TelemetryExportTest is POST /telemetry/export/test: whether the collector
// accepted one test span sent with the draft setting.
type TelemetryExportTest struct {
	OK bool `json:"ok"`
	// TraceID is the test span's trace, to search for in the tracing
	// backend; TracesURL where it was posted.
	TraceID   string `json:"trace_id"`
	TracesURL string `json:"traces_url"`
	// Error is why it was not accepted; empty when it was.
	Error string `json:"error"`
}
