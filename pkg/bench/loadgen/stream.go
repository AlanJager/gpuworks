package loadgen

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

func parseSSE(r io.Reader, started time.Time) (Result, error) {
	return parseSSEWithClock(r, started, time.Now)
}

func parseSSEWithClock(r io.Reader, started time.Time, now func() time.Time) (out Result, err error) {
	out.Metrics.StartedAt = started
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 4<<20)
	var pieces, generated []string
	complete := false
	defer func() {
		out.Text = strings.Join(pieces, "")
		out.generatedText = strings.Join(generated, "")
		if out.Metrics.FinishedAt.IsZero() {
			out.Metrics.FinishedAt = now()
		}
		if !out.Metrics.LastOutputAt.IsZero() {
			out.Metrics.Latency = out.Metrics.LastOutputAt.Sub(started)
		}
		applyServerUsage(&out)
		if out.Metrics.OutputSource == "" {
			out.Metrics.OutputTokens = (len([]rune(out.generatedText)) + 3) / 4
			out.Metrics.OutputSource = "character_estimate"
			out.Metrics.ApproximateTokens = true
		}
		if out.Metrics.OutputTokens > 1 && out.Metrics.Chunks > 1 {
			out.Metrics.TPOT = out.Metrics.LastOutputAt.Sub(out.Metrics.FirstOutputAt)
		}
		if err != nil {
			out.Metrics.Error = err
			out.Metrics.ErrorStage = "stream"
		}
	}()
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			complete = true
			out.Metrics.FinishedAt = now()
			break
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					Thinking         string `json:"thinking"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if decodeErr := json.Unmarshal([]byte(data), &ev); decodeErr != nil {
			return out, fmt.Errorf("invalid stream event: %w", decodeErr)
		}
		eventTime := now()
		if len(ev.Error) > 0 && string(ev.Error) != "null" {
			return out, fmt.Errorf("endpoint stream error: %s", string(ev.Error))
		}
		content, reasoning := "", ""
		if len(ev.Choices) > 0 {
			choice := ev.Choices[0]
			content, reasoning = choice.Delta.Content, reasoningDelta(choice.Delta.ReasoningContent, choice.Delta.Reasoning, choice.Delta.Thinking)
			if choice.FinishReason != nil {
				out.Metrics.FinishReason = *choice.FinishReason
				complete = *choice.FinishReason != ""
			}
		}
		if content != "" && out.Metrics.FirstContentAt.IsZero() {
			out.Metrics.FirstContentAt = eventTime
		}
		if content != "" || reasoning != "" {
			if out.Metrics.FirstOutputAt.IsZero() {
				out.Metrics.FirstOutputAt = eventTime
				out.Metrics.TTFT = eventTime.Sub(started)
			}
			if !out.Metrics.LastOutputAt.IsZero() {
				out.Metrics.ITL = append(out.Metrics.ITL, eventTime.Sub(out.Metrics.LastOutputAt))
			}
			out.Metrics.LastOutputAt = eventTime
			out.Metrics.Chunks++
			pieces = append(pieces, content)
			generated = append(generated, reasoning, content)
		}
		if len(ev.Usage) > 0 {
			out.UsageJSON = append(json.RawMessage(nil), ev.Usage...)
			applyServerUsage(&out)
		}
	}
	if scanErr := scan.Err(); scanErr != nil {
		return out, scanErr
	}
	if !complete {
		return out, io.ErrUnexpectedEOF
	}
	return out, nil
}

// reasoningDelta returns a chunk's (or a message's) reasoning text. Engines name the field
// differently (SGLang and older vLLM reasoning_content, vLLM 0.34 reasoning,
// some gateways thinking), and transitional vLLM releases send the same text
// under two names, so the first non-empty one is the chunk's reasoning rather
// than their concatenation. The gateway's usage observer accepts the same set.
func reasoningDelta(names ...string) string {
	for _, text := range names {
		if text != "" {
			return text
		}
	}
	return ""
}

// applyServerUsage records presence independently for each field. An explicit
// zero is a usable observation; null/absent is not, including cached_tokens.
func applyServerUsage(out *Result) {
	var v struct {
		PromptTokens        *int `json:"prompt_tokens"`
		CompletionTokens    *int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens *int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens *int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if len(out.UsageJSON) == 0 || json.Unmarshal(out.UsageJSON, &v) != nil {
		return
	}
	_ = json.Unmarshal(out.UsageJSON, &out.RawUsage)
	if v.PromptTokens != nil && *v.PromptTokens >= 0 {
		out.Metrics.ServerInputTokens = v.PromptTokens
		out.Metrics.InputTokens, out.Metrics.InputSource = *v.PromptTokens, "server_usage"
	}
	if v.CompletionTokens != nil && *v.CompletionTokens >= 0 {
		out.Metrics.ServerOutputTokens = v.CompletionTokens
		out.Metrics.OutputTokens, out.Metrics.OutputSource = *v.CompletionTokens, "server_usage"
	}
	if v.PromptTokensDetails != nil && v.PromptTokensDetails.CachedTokens != nil && *v.PromptTokensDetails.CachedTokens >= 0 {
		out.Metrics.CachedTokens, out.Metrics.CachedTokensAvailable = *v.PromptTokensDetails.CachedTokens, true
	}
	if v.CompletionTokensDetails != nil && v.CompletionTokensDetails.ReasoningTokens != nil && *v.CompletionTokensDetails.ReasoningTokens >= 0 {
		out.Metrics.ReasoningTokens = v.CompletionTokensDetails.ReasoningTokens
	}
}

func addLocalCounts(out *Result, req Request, count func(string) int) {
	source := "local_tokenizer_estimate"
	if count == nil {
		source = "character_estimate"
		count = func(s string) int { return (len([]rune(s)) + 3) / 4 }
	}
	input := 0
	for _, m := range req.Messages {
		if m.Content != "" {
			input += count(m.Content)
		}
	}
	text := out.generatedText
	if text == "" {
		text = out.Text
	}
	output := 0
	if text != "" {
		output = count(text)
	}
	out.Metrics.LocalInputTokens, out.Metrics.LocalOutputTokens = &input, &output
	if out.Metrics.ServerInputTokens == nil {
		out.Metrics.InputTokens, out.Metrics.InputSource = input, source
	}
	if out.Metrics.ServerOutputTokens == nil {
		out.Metrics.OutputTokens, out.Metrics.OutputSource = output, source
	}
	out.Metrics.ApproximateTokens = out.Metrics.InputSource != "server_usage" || out.Metrics.OutputSource != "server_usage"
	if out.Metrics.Chunks > 1 && !out.Metrics.FirstOutputAt.IsZero() && !out.Metrics.LastOutputAt.IsZero() {
		out.Metrics.TPOT = out.Metrics.LastOutputAt.Sub(out.Metrics.FirstOutputAt)
	}
}
