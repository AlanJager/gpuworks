package dto

// AIConfigView is one AI service as the console lists it. The upstream API key
// is never part of it: it is written once and never read back.
type AIConfigView struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	Preset    string `json:"preset"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	IsDefault bool   `json:"is_default"`
}

// AIConfigRequest adds an AI service (POST /api/v1/ai/configs). A known preset
// fills base_url and model when they are left empty. An empty api_key is
// allowed: a model served inside the installation often asks for none.
type AIConfigRequest struct {
	Name      string `json:"name"`
	Preset    string `json:"preset"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	Model     string `json:"model"`
	IsDefault bool   `json:"is_default"`
}

// AIConfigPatch changes an AI service (PATCH /api/v1/ai/configs/{id}). Only the
// fields present are changed, so a form that leaves the key alone cannot drop
// it. api_key is the one field where an empty string means "keep the stored
// key": an operator editing a name must not have to retype a secret the
// platform never showed them.
type AIConfigPatch struct {
	Name      *string `json:"name,omitempty"`
	Preset    *string `json:"preset,omitempty"`
	BaseURL   *string `json:"base_url,omitempty"`
	APIKey    *string `json:"api_key,omitempty"`
	Model     *string `json:"model,omitempty"`
	IsDefault *bool   `json:"is_default,omitempty"`
}

// AIConfigTest asks whether an AI service answers (POST /api/v1/ai/configs/test),
// about the values a form holds rather than about a stored service: nothing is
// saved, so a wrong address costs nothing to try. ID names the stored service
// an edit dialog started from; with an empty api_key the stored key is used,
// but only while base_url still matches the stored address - a probe must not
// be a way to send a stored key to an address of the caller's choosing.
type AIConfigTest struct {
	ID      uint   `json:"id,omitempty"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model"`
}

// AIConfigTestResult is what the probe established. Outcome is a stable code
// the client words in its own language; Detail is the endpoint's own short
// answer (never the key) and Status its HTTP status when it spoke at all.
// BaseURL is the address the probe actually called, normalization applied.
type AIConfigTestResult struct {
	Outcome   string `json:"outcome"`
	Status    int    `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
	BaseURL   string `json:"base_url"`
	LatencyMS int64  `json:"latency_ms"`
}
