package metrics

import "time"

// PointEvidence is a versioned, bounded explanation of one measured point.
// It is independent of optional request bodies and per-request detail files.
type PointEvidence struct {
	Version           int                     `json:"version"`
	Plan              EvidencePlan            `json:"plan"`
	Outcomes          EvidenceOutcomes        `json:"outcomes"`
	All               RequestEvidence         `json:"all"`
	FirstTurn         RequestEvidence         `json:"first_turn"`
	FollowupTurns     RequestEvidence         `json:"followup_turns"`
	ByTurn            map[int]RequestEvidence `json:"by_turn"`
	Timing            EvidenceTiming          `json:"timing"`
	Execution         EvidenceExecution       `json:"execution"`
	Sessions          []SessionEvidence       `json:"sessions"`
	SessionsTruncated bool                    `json:"sessions_truncated"`
}
type EvidencePlan struct {
	PlannedSessions int    `json:"planned_sessions"`
	PlannedRequests int    `json:"planned_requests"`
	TurnsPerSession int    `json:"turns_per_session,omitempty"`
	OutputBudget    int    `json:"output_budget"`
	OutputPolicy    string `json:"output_policy"`
}
type EvidenceOutcomes struct {
	Skipped             int `json:"skipped"`
	CanceledBeforeStart int `json:"canceled_before_start"`
	CompletedSessions   int `json:"completed_sessions"`
	FailedSessions      int `json:"failed_sessions"`
	CanceledSessions    int `json:"canceled_sessions"`
	RetryCount          int `json:"retry_count"`
}
type RequestEvidence struct {
	Attempted         int            `json:"attempted"`
	Success           int            `json:"success"`
	Failed            int            `json:"failed"`
	Canceled          int            `json:"canceled"`
	TTFT              MetricSummary  `json:"ttft"`
	TPOT              MetricSummary  `json:"tpot"`
	LastOutputLatency MetricSummary  `json:"last_output_latency"`
	Completion        MetricSummary  `json:"completion"`
	Tokens            TokenEvidence  `json:"tokens"`
	Cache             CacheEvidence  `json:"cache"`
	FinishReasons     map[string]int `json:"finish_reasons"`
	ErrorStages       map[string]int `json:"error_stages"`
}
type MetricSummary struct {
	Count  int     `json:"count"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P99Ms  float64 `json:"p99_ms"`
}
type TokenDistribution struct {
	Count int     `json:"count"`
	Total int64   `json:"total"`
	Min   int     `json:"min"`
	Max   int     `json:"max"`
	P50   float64 `json:"p50"`
	P99   float64 `json:"p99"`
}
type TokenEvidence struct {
	Input         TokenDistribution `json:"input"`
	Output        TokenDistribution `json:"output"`
	Context       TokenDistribution `json:"context"`
	ServerInput   TokenDistribution `json:"server_input"`
	ServerOutput  TokenDistribution `json:"server_output"`
	LocalInput    TokenDistribution `json:"local_input"`
	LocalOutput   TokenDistribution `json:"local_output"`
	Reasoning     TokenDistribution `json:"reasoning"`
	FailedOutput  TokenDistribution `json:"failed_output"`
	InputSources  map[string]int    `json:"input_sources"`
	OutputSources map[string]int    `json:"output_sources"`
}
type CacheEvidence struct {
	Reported           int   `json:"reported"`
	Unknown            int   `json:"unknown"`
	RatioRequests      int   `json:"ratio_requests"`
	SuccessfulReported int   `json:"successful_reported"`
	InputTokens        int64 `json:"input_tokens"`
	CachedTokens       int64 `json:"cached_tokens"`
}
type EvidenceTiming struct {
	StartedAt               *time.Time    `json:"started_at"`
	FinishedAt              *time.Time    `json:"finished_at"`
	CompletedSessionLatency MetricSummary `json:"completed_session_latency"`
}
type EvidenceExecution struct {
	Seed                  int64  `json:"seed,string"`
	SeedDerivation        string `json:"seed_derivation"`
	CachePolicy           string `json:"cache_policy"`
	CacheState            string `json:"cache_state"`
	RetryPolicy           string `json:"retry_policy"`
	TokenizerSource       string `json:"tokenizer_source"`
	LocalTokenCountMethod string `json:"local_token_count_method"`
	DetailPolicy          string `json:"detail_policy"`
	WarmupRequests        int    `json:"warmup_requests"`
	WarmupSucceeded       int    `json:"warmup_succeeded"`
}
type SessionEvidence struct {
	SessionID    string     `json:"session_id"`
	PlannedTurns int        `json:"planned_turns"`
	Attempted    int        `json:"attempted"`
	Success      int        `json:"success"`
	Failed       int        `json:"failed"`
	Canceled     int        `json:"canceled"`
	Skipped      int        `json:"skipped"`
	State        string     `json:"state"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}
