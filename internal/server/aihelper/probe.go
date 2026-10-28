package aihelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/outbound"
)

// probeDetailRunes bounds the endpoint's own words in a probe answer: enough
// to recognise the failure, never a whole HTML error page.
const probeDetailRunes = 300

// ProbeOutcome is what asking a chat endpoint established, as a stable code
// the client renders in its own language. The distinctions that matter are
// the ones an operator acts on: a wrong address, a wrong key, a model the
// endpoint does not serve, and an endpoint that is simply not reachable.
type ProbeOutcome string

const (
	// ProbeOK: the endpoint answered with a completion.
	ProbeOK ProbeOutcome = "ok"
	// ProbeConnection: DNS, dial, TLS or a timeout - the address does not
	// answer at all.
	ProbeConnection ProbeOutcome = "connection"
	// ProbeBlocked: the platform's outbound policy refused the address (a
	// private host no administrator allowed). Decision, not a network fact.
	ProbeBlocked ProbeOutcome = "blocked"
	// ProbeAuth: 401/403 - the endpoint answered, the key is wrong or missing.
	ProbeAuth ProbeOutcome = "auth"
	// ProbePath: a 404 that says nothing about a model - the address is
	// wrong, typically the call path was included twice.
	ProbePath ProbeOutcome = "path"
	// ProbeModel: the endpoint answered that it does not serve Model.
	ProbeModel ProbeOutcome = "model"
	// ProbeHTTP: any other status, or a 2xx that is not a completion.
	ProbeHTTP ProbeOutcome = "http"
)

// ProbeResult is one probe's answer. Status and Detail are the endpoint's own
// words where it spoke; Detail never contains the API key.
type ProbeResult struct {
	Outcome   ProbeOutcome
	Status    int
	Detail    string
	LatencyMS int64
}

// Probe asks the endpoint for a one-word completion with the endpoint's own
// model and key, the same call Chat makes, so what it finds out is what the
// assistant would hit. It stores nothing and never returns the key. The
// caller bounds it with a context; there is no retry, so a wrong address is
// reported at once.
func (c *Client) Probe(ctx context.Context) ProbeResult {
	started := time.Now()
	body, err := json.Marshal(ChatRequest{Model: c.ep.Model, Messages: []Message{{Role: "user", Content: "ping"}}})
	if err != nil {
		return ProbeResult{Outcome: ProbeConnection, Detail: "the request could not be built", LatencyMS: millisSince(started)}
	}
	call, err := ChatCompletionsURL(c.ep.BaseURL)
	if err != nil {
		return ProbeResult{Outcome: ProbeConnection, Detail: c.redact(err.Error()), LatencyMS: millisSince(started)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, call, bytes.NewReader(body))
	if err != nil {
		return ProbeResult{Outcome: ProbeConnection, Detail: c.redact(err.Error()), LatencyMS: millisSince(started)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.ep.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return ProbeResult{Outcome: outboundFailure(err), Detail: c.redact(transportDetail(err)), LatencyMS: millisSince(started)}
	}
	defer c.http.CloseIdleConnections()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	answered := ProbeResult{Status: resp.StatusCode, Detail: c.redact(boundedText(raw)), LatencyMS: millisSince(started)}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var cr ChatResponse
		if json.Unmarshal(raw, &cr) == nil && len(cr.Choices) > 0 {
			answered.Outcome, answered.Detail = ProbeOK, ""
			return answered
		}
		answered.Outcome, answered.Detail = ProbeHTTP, "the endpoint answered "+http.StatusText(resp.StatusCode)+" but not with a completion"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		answered.Outcome = ProbeAuth
	case resp.StatusCode == http.StatusNotFound:
		// A 404 is the wrong address unless the endpoint names the model:
		// a gateway answers {"detail":"Not Found"}, a model server answers
		// "no deployment serves model ...".
		answered.Outcome = ProbePath
		if namesModel(raw) {
			answered.Outcome = ProbeModel
		}
	default:
		// A model the endpoint does not serve is reported as 400 or 422 by
		// some servers; keep that apart from a general HTTP failure.
		answered.Outcome = ProbeHTTP
		if namesModel(raw) {
			answered.Outcome = ProbeModel
		}
	}
	return answered
}

// outboundFailure keeps the policy's own refusal apart from a network fact:
// the operator fixes the first in the allowlist, the second at the provider.
func outboundFailure(err error) ProbeOutcome {
	if errors.Is(err, outbound.ErrTargetDenied) {
		return ProbeBlocked
	}
	return ProbeConnection
}

// transportDetail says what can be said about a failed call. The outbound
// transport deliberately hides DNS answers, socket addresses and proxy
// credentials behind one sentence; a timeout is the exception worth keeping.
func transportDetail(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "the endpoint did not answer in time"
	}
	if errors.Is(err, context.Canceled) {
		return "the request was cancelled"
	}
	return ""
}

// namesModel reports whether an endpoint's error body is about the model it
// was asked for rather than about the address being wrong. Servers word it
// differently ("no deployment serves model x", "The model `x` does not
// exist.", {"code":"model_not_found"}), so the test is the word model next to
// a word of absence.
func namesModel(raw []byte) bool {
	text := strings.ToLower(string(raw))
	if !strings.Contains(text, "model") {
		return false
	}
	for _, absent := range []string{"not found", "not_found", "does not exist", "no deployment", "unknown model", "invalid model", "unsupported model", "no model"} {
		if strings.Contains(text, absent) {
			return true
		}
	}
	return false
}

// boundedText is an error body as one short line of the endpoint's words.
func boundedText(raw []byte) string {
	text := strings.Join(strings.Fields(string(raw)), " ")
	if runes := []rune(text); len(runes) > probeDetailRunes {
		return string(runes[:probeDetailRunes])
	}
	return text
}

// redact keeps the key out of anything an endpoint echoes back.
func (c *Client) redact(text string) string {
	if c.ep.APIKey == "" {
		return text
	}
	return strings.ReplaceAll(text, c.ep.APIKey, "***")
}

func millisSince(started time.Time) int64 {
	return time.Since(started).Milliseconds()
}
