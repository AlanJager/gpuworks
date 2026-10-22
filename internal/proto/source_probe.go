package proto

// RPCSourceProbe asks a node whether it reaches the addresses a site rule
// offers, the way its downloads would.
const RPCSourceProbe = "source.probe"

// SourceProbeParams names the addresses to try (bases of a hub such as
// Hugging Face or ModelScope) and the repository each is asked about.
type SourceProbeParams struct {
	Source string   `json:"source"` // huggingface | modelscope
	Repo   string   `json:"repo"`
	Bases  []string `json:"bases"`
}

// Outcomes of probing one address.
const (
	SourceProbeOK           = "ok"
	SourceProbeAuthRequired = "auth_required"
	SourceProbeHTTPError    = "http_error"
	SourceProbeUnreachable  = "unreachable"
)

// SourceProbeOutcome is how one address answered: Status, the HTTP Code when
// it answered, how long it took, and the error when it did not.
type SourceProbeOutcome struct {
	Base      string `json:"base"`
	Status    string `json:"status"`
	Code      int    `json:"code,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// SourceProbeResult is every address's outcome, in the order asked.
type SourceProbeResult struct {
	Outcomes []SourceProbeOutcome `json:"outcomes"`
}
