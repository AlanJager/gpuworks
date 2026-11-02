package experience

import (
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/simulate"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// caller is an application calling the platform with its own API key. The
// shapes differ the way real workloads do: a knowledge-base assistant sends
// long, highly cacheable prompts; a copywriting tool short prompts and long
// answers.
type caller struct {
	Name string
	// Share is the caller's part of the platform's requests.
	Share float64
	// Prompt and Output are typical token counts; Cache is the part of the
	// prompt a prefix cache serves.
	Prompt, Output int
	Cache          float64
}

var callers = []caller{
	{Name: "智能客服", Share: 0.38, Prompt: 1100, Output: 220, Cache: 0.48},
	{Name: "知识库问答", Share: 0.30, Prompt: 3800, Output: 380, Cache: 0.72},
	{Name: "代码助手", Share: 0.20, Prompt: 5200, Output: 520, Cache: 0.58},
	{Name: "运营文案", Share: 0.12, Prompt: 1400, Output: 680, Cache: 0.28},
}

// callerKeys creates each caller's API key once and returns name -> key.
func callerKeys(ctx context.Context, c *apiClient, path string) (map[string]string, error) {
	keys := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &keys)
	}
	changed := false
	for _, cl := range callers {
		if keys[cl.Name] != "" {
			continue
		}
		var out struct {
			Key string `json:"key"`
		}
		if err := c.post(ctx, "/api/v1/apikeys", map[string]string{"name": cl.Name}, &out); err != nil {
			return nil, err
		}
		keys[cl.Name], changed = out.Key, true
	}
	if changed {
		raw, _ := json.Marshal(keys)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// requestHistory is 30 days of requests for the starter deployments, each
// from the day it went live: the working-day rhythm (simulate.Rhythm),
// steady growth, cache hits and misses, and the small share of errors,
// truncations and abandoned requests a busy platform has. ids are the
// deployments' by name; keys the callers' API keys (id, user) by name.
func requestHistory(start, end time.Time, ids map[string]uint, keys map[string][2]uint) ([]uxrecord.Request, []int64) {
	rng := rand.New(rand.NewPCG(uint64(start.Unix()), 7))
	var recs []uxrecord.Request
	var weights []int64
	add := func(rec uxrecord.Request, weight float64) {
		if n := int64(math.Round(weight)); n > 0 {
			recs, weights = append(recs, rec), append(weights, n)
		}
	}
	for hour := start; hour.Before(end); hour = hour.Add(time.Hour) {
		r := simulate.Rhythm(hour.Add(30 * time.Minute))
		load := peakRequestsPerHour * r * (0.72 + 0.28*hour.Sub(start).Hours()/(30*24)) * (0.88 + rng.Float64()*0.24)
		for _, st := range starters {
			if ids[st.Name] == 0 || hour.Before(end.AddDate(0, 0, -st.Days)) {
				continue
			}
			for _, cl := range callers {
				id := keys[cl.Name]
				if id[0] == 0 {
					continue
				}
				n := load * st.Share * cl.Share
				if n < 0.5 {
					continue
				}
				prompt := jitter(rng, cl.Prompt, 0.2)
				output := jitter(rng, cl.Output, 0.25)
				cached := int(float64(prompt) * math.Min(0.95, cl.Cache*(0.85+rng.Float64()*0.3)))
				// Latency grows with the hour's load, as queues do.
				pressure := 1 + 0.35*r
				ttft := st.TTFT * pressure * (0.9 + rng.Float64()*0.2)
				tpot := st.TPOT * pressure * (0.92 + rng.Float64()*0.16)
				rec := uxrecord.Request{
					ReceivedMS:   hour.Add(time.Duration(rng.IntN(3600)) * time.Second).UnixMilli(),
					DeploymentID: ids[st.Name], APIKeyID: id[0], UserID: id[1], Source: uxrecord.SourceAPI,
					Model: st.Name, Path: "/v1/chat/completions", Stream: true, Status: 200,
					Outcome: uxrecord.OutcomeOK, FinishReason: "stop", Usage: true,
					PromptTokens: int32(prompt), CompletionTokens: int32(output), CachedTokens: int32(cached),
					ReasoningTokens: uxrecord.Unknown, FirstThinkMS: uxrecord.Unknown,
					FirstByteMS: int32(ttft - 10), FirstAnswerMS: int32(ttft),
					LastChunkMS: int32(ttft + tpot*float64(output)), EndMS: int32(ttft + tpot*float64(output) + 8),
					Weight: 1,
				}
				add(rec, n*0.968)
				long := rec
				long.FinishReason, long.CompletionTokens = "length", int32(output*2)
				long.LastChunkMS = int32(ttft + tpot*float64(output*2))
				long.EndMS = long.LastChunkMS + 8
				add(long, n*0.02)
				closed := rec
				closed.Outcome, closed.CompletionTokens = uxrecord.OutcomeClientClosed, int32(output/3)
				closed.EndMS = int32(ttft + tpot*float64(output/3))
				closed.LastChunkMS = closed.EndMS
				add(closed, n*0.006)
				failed := uxrecord.Request{
					ReceivedMS: rec.ReceivedMS, DeploymentID: rec.DeploymentID, APIKeyID: id[0], UserID: id[1],
					Source: uxrecord.SourceAPI, Model: st.Name, Path: "/v1/chat/completions", Stream: true,
					Status: 503, Outcome: uxrecord.OutcomeUpstreamError, Usage: false,
					PromptTokens: uxrecord.Unknown, CompletionTokens: uxrecord.Unknown, CachedTokens: uxrecord.Unknown,
					ReasoningTokens: uxrecord.Unknown, FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: uxrecord.Unknown,
					FirstByteMS: uxrecord.Unknown, LastChunkMS: uxrecord.Unknown, EndMS: 30, Weight: 1,
				}
				add(failed, n*0.006)
			}
		}
	}
	return recs, weights
}

// peakRequestsPerHour is the platform's requests in its busiest hour.
const peakRequestsPerHour = 9000

func jitter(rng *rand.Rand, n int, spread float64) int {
	return max(1, int(float64(n)*(1-spread+rng.Float64()*2*spread)))
}

// liveContext is the shared prefix a caller's live requests carry (its
// system prompt and retrieved context), sized to the caller's typical
// prompt, so the engine's prefix cache has something to hit.
func liveContext(cl caller) string {
	words := strings.Fields(tokenizerWords)
	var b strings.Builder
	for i := 0; i < cl.Prompt*3/4; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(words[(i*7+len(cl.Name))%len(words)])
	}
	return b.String()
}
