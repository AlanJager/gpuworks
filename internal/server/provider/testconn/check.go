// Package testconn runs provider connection tests: layered checks from input
// shape through network reachability to vendor authentication, each reported
// as a typed Check so failures localize to one layer.
package testconn

import (
	"context"
	"fmt"
)

// Status is one check's outcome.
type Status string

const (
	StatusOK      Status = "ok"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped"
)

// Stable wire identifiers. Never rename without a compatibility note.
const (
	CheckEndpointFormat   = "endpoint_format"
	CheckCredentialsShape = "credentials_shape"
	CheckDNSResolve       = "dns_resolve"
	CheckTCPConnect       = "tcp_connect"
	CheckTLSHandshake     = "tls_handshake"
	CheckProxyDetect      = "proxy_detect"
	CheckClockSkew        = "clock_skew"
	CheckAuthRequest      = "auth_request"
	CheckProtocolShape    = "protocol_shape"
	CheckAPIEndpoints     = "api_endpoints"
	CheckReadPermissions  = "read_permissions"
	CheckResourceSummary  = "resource_summary"
)

// Hint keys a client maps to guidance. A skipped check carries the reason it
// did not run: HintDependsFailed blocks whatever depends on it, the other two
// do not.
const (
	HintDependsFailed = "depends_failed"
	// HintNotApplicable skips a check that has nothing to examine, such as the
	// endpoint checks of a kind whose driver picks its endpoints itself.
	HintNotApplicable = "not_applicable"
	// HintViaProxy skips the TLS handshake of an endpoint reached through a
	// proxy: the server's own connection ends at the proxy.
	HintViaProxy = "via_proxy"
	HintProxy    = "proxy"
	HintAuth     = "auth"
)

// Check is one item of a connection-test report.
type Check struct {
	Seq        int            `json:"seq"`
	Name       string         `json:"name"`
	Status     Status         `json:"status"`
	DurationMS int64          `json:"duration_ms"`
	Code       string         `json:"code,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
	Hint       string         `json:"hint,omitempty"`
}

// Summary ends every run. Failed lists the failed checks in report order.
type Summary struct {
	OK     bool     `json:"ok"`
	Failed []string `json:"failed,omitempty"`
}

// ReportFunc receives each finished check.
type ReportFunc func(Check)

// Failure fails a check with a stable product code and a hint key. Err is the
// cause; it is never serialized onto the wire, only logged on the server
// through Input.Redact.
type Failure struct {
	Code   string
	Hint   string
	Detail map[string]any
	Err    error
}

func (f *Failure) Error() string { return fmt.Sprintf("%s: %v", f.Code, f.Err) }

// VendorCheck is one driver-specific check.
type VendorCheck struct {
	Name    string
	Depends []string
	Run     func(ctx context.Context) (map[string]any, error)
}

// Input describes one connection test.
type Input struct {
	Kind     string
	Endpoint string
	// EndpointNotApplicable marks a kind that takes no endpoint (its driver
	// picks them): the endpoint and network checks are skipped as not
	// applicable and the vendor checks still run.
	EndpointNotApplicable bool
	// RequiredCredentials lists credential field names that must be non-empty.
	RequiredCredentials  []string
	CredentialsSatisfied func(name string) bool
	Vendor               []VendorCheck
	// Redact removes credential values from a failure's cause before it is
	// logged. The runner never sees the credentials themselves; nil logs the
	// cause as it is.
	Redact func(string) string
}
