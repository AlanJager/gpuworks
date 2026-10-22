package dto

import "time"

// UsageReportStatus is GET /api/v1/usage-report: whether the installation
// takes part in the user experience improvement program and how its last
// daily report went.
type UsageReportStatus struct {
	// Enabled is usage_report in server.yaml (absent means on); Editable
	// says the console can switch it (the server keeps it in server.yaml).
	Enabled  bool `json:"enabled"`
	Editable bool `json:"editable"`
	// LastDay is the UTC day (YYYY-MM-DD) of the last report delivered;
	// empty before the first.
	LastDay string `json:"last_day,omitempty"`
	// LastAttempt is the latest delivery attempt since the server started;
	// absent when none was made.
	LastAttempt *UsageReportAttempt `json:"last_attempt,omitempty"`
	// NextDueAt is when the next report is due; absent when the program is
	// off.
	NextDueAt *time.Time `json:"next_due_at,omitempty"`
}

// UsageReportAttempt is one delivery attempt.
type UsageReportAttempt struct {
	At  time.Time `json:"at"`
	Day string    `json:"day"`
	// OK says an endpoint took the report.
	OK bool `json:"ok"`
	// Error is why no endpoint took it.
	Error string `json:"error,omitempty"`
}

// UsageReportEnabled is PUT /api/v1/usage-report/enabled.
type UsageReportEnabled struct {
	Enabled *bool `json:"enabled"`
}

// NoticeList is GET /api/v1/notices: the notices for this installation,
// most urgent first, and how many the caller has not read.
type NoticeList struct {
	Notices []Notice `json:"notices"`
	Unread  int      `json:"unread"`
}

// Notice is one of the publisher's messages for the console's notice bell:
// Kind is security, release or event.
type Notice struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	// Title and Body are the publisher's plain text per locale ("en" always
	// present).
	Title map[string]string `json:"title,omitempty"`
	Body  map[string]string `json:"body,omitempty"`
	// URL is an https link to read more.
	URL         string    `json:"url,omitempty"`
	PublishedAt time.Time `json:"published_at"`
	Read        bool      `json:"read"`
}

// NoticesRead is POST /api/v1/notices/read: the notices the caller has read.
type NoticesRead struct {
	IDs []string `json:"ids"`
}
