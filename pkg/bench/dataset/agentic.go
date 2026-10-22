package dataset

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"

	"gpuworks.ai/gpuworks/pkg/bench/tokenizer"
)

const (
	AgenticGeneratorVersion                  = 1
	AgenticDefaultPreset                     = "agentic-80k-v1"
	AgenticDefaultTurns                      = 13
	AgenticSubsequentTurnInputTokens         = 753
	AgenticOutputTokens                      = 220
	AgenticLengthDriftWarningThreshold       = 0.10
	AgenticDefaultSeed                 int64 = 42
)

func AgenticLengthDriftExceeded(actual float64, target int) bool {
	return target > 0 && math.Abs(actual-float64(target))/float64(target) > AgenticLengthDriftWarningThreshold
}

type AgenticPreset struct {
	Value                 string `json:"value"`
	Label                 string `json:"label"`
	MeanInputTokens       int    `json:"mean_input_tokens"`
	FirstTurnInputTokens  int    `json:"first_turn_input_tokens"`
	PeakInputTokens       int    `json:"peak_input_tokens"`
	RequiredContextTokens int    `json:"required_context_tokens"`
	Recommended           bool   `json:"recommended,omitempty"`
}

// agenticPresets is the length ladder offered to users, ordered by mean input
// length. Every row is a fixed point of ExpandAgenticConfig's derivation (the
// mean it recomputes from the first turn equals the mean it was given), so the
// table and the generator can never describe two different workloads;
// TestAgenticPresetsMatchTheGeneratorFormula re-derives each row.
var agenticPresets = []AgenticPreset{
	{Value: "agentic-10k-v1", Label: "10K", MeanInputTokens: 10_000, FirstTurnInputTokens: 4_162, PeakInputTokens: 15_838, RequiredContextTokens: 20_000},
	{Value: "agentic-20k-v1", Label: "20K", MeanInputTokens: 20_000, FirstTurnInputTokens: 14_162, PeakInputTokens: 25_838, RequiredContextTokens: 30_000},
	{Value: "agentic-40k-v1", Label: "40K", MeanInputTokens: 40_000, FirstTurnInputTokens: 34_162, PeakInputTokens: 45_838, RequiredContextTokens: 50_000},
	{Value: AgenticDefaultPreset, Label: "80K", MeanInputTokens: 80_000, FirstTurnInputTokens: 74_162, PeakInputTokens: 85_838, RequiredContextTokens: 90_000, Recommended: true},
	{Value: "agentic-128k-v1", Label: "128K", MeanInputTokens: 128_000, FirstTurnInputTokens: 122_162, PeakInputTokens: 133_838, RequiredContextTokens: 138_000},
	{Value: "agentic-256k-v1", Label: "256K", MeanInputTokens: 256_000, FirstTurnInputTokens: 250_162, PeakInputTokens: 261_838, RequiredContextTokens: 266_000},
	{Value: "agentic-512k-v1", Label: "512K", MeanInputTokens: 512_000, FirstTurnInputTokens: 506_162, PeakInputTokens: 517_838, RequiredContextTokens: 522_000},
	{Value: "agentic-1m-v1", Label: "1M", MeanInputTokens: 1_000_000, FirstTurnInputTokens: 994_162, PeakInputTokens: 1_005_838, RequiredContextTokens: 1_010_000},
}

func AgenticPresets() []AgenticPreset { return append([]AgenticPreset(nil), agenticPresets...) }

func ExpandAgenticConfig(c Config) (Config, error) {
	if c.Kind != "agentic" {
		return c, fmt.Errorf("dataset kind must be agentic")
	}
	if c.Preset == "" {
		c.Preset = AgenticDefaultPreset
	}
	var preset *AgenticPreset
	for i := range agenticPresets {
		if agenticPresets[i].Value == c.Preset {
			preset = &agenticPresets[i]
			break
		}
	}
	if preset == nil {
		return c, fmt.Errorf("unknown agentic preset %q", c.Preset)
	}
	if c.MeanInputTokens <= 0 {
		c.MeanInputTokens = preset.MeanInputTokens
	}
	if c.Turns <= 0 {
		c.Turns = AgenticDefaultTurns
	}
	if c.Turns < 2 || c.Turns > 64 {
		return c, fmt.Errorf("agentic turns must be between 2 and 64")
	}
	if err := ValidateAllocation(Config{InputTokens: c.MeanInputTokens}, 1); err != nil {
		return c, err
	}
	c.GeneratorVersion = AgenticGeneratorVersion
	c.SubsequentTurnInputTokens = AgenticSubsequentTurnInputTokens
	c.OutputTokens = AgenticOutputTokens
	if c.Seed == 0 {
		c.Seed = AgenticDefaultSeed
	}
	growth := c.SubsequentTurnInputTokens + c.OutputTokens
	c.FirstTurnInputTokens = int(math.Round(float64(c.MeanInputTokens) - float64(c.Turns-1)*float64(growth)/2))
	if c.FirstTurnInputTokens <= 0 {
		return c, fmt.Errorf("agentic first prompt is not positive for mean_input_tokens=%d and turns=%d", c.MeanInputTokens, c.Turns)
	}
	c.PeakInputTokens = c.FirstTurnInputTokens + (c.Turns-1)*growth
	c.RequiredContextTokens = int(math.Ceil(float64(c.PeakInputTokens+c.OutputTokens+3_000)/1_000)) * 1_000
	var total, reused int64
	for turn := 0; turn < c.Turns; turn++ {
		prompt := c.FirstTurnInputTokens + turn*growth
		total += int64(prompt)
		if turn > 0 {
			reused += int64(prompt - c.SubsequentTurnInputTokens)
		}
	}
	c.MeanInputTokens = int(math.Round(float64(total) / float64(c.Turns)))
	c.InputTokens = c.MeanInputTokens // retained for existing point/result consumers
	c.DesignPrefixReuseRate = float64(reused) / float64(total)
	c.WorkloadFingerprint = agenticFingerprint(c)
	return c, nil
}

func agenticFingerprint(c Config) string {
	wire := struct {
		Kind, Preset                                                                                            string
		GeneratorVersion, MeanInputTokens, FirstTurnInputTokens, SubsequentTurnInputTokens, OutputTokens, Turns int
		Seed                                                                                                    int64
	}{c.Kind, c.Preset, c.GeneratorVersion, c.MeanInputTokens, c.FirstTurnInputTokens, c.SubsequentTurnInputTokens, c.OutputTokens, c.Turns, c.Seed}
	b, _ := json.Marshal(wire)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func agentic(c Config, n int, tok *tokenizer.Tokenizer) ([]Sample, error) {
	if tok == nil {
		return nil, fmt.Errorf("agentic workload requires an exact tokenizer")
	}
	expanded, err := ExpandAgenticConfig(c)
	if err != nil {
		return nil, err
	}
	out := make([]Sample, n)
	for i := range out {
		// Each session owns an independent deterministic stream. The first sampled
		// tokens differ from the beginning, avoiding cross-session/cache pollution.
		r := rand.New(rand.NewSource(expanded.Seed + int64(i+1)*1_000_003))
		sessionID := fmt.Sprintf("session-%016x\n", uint64(expanded.Seed)+uint64(i+1)*1_000_003)
		followUps := make([]Message, expanded.Turns-1)
		for turn := range followUps {
			followUps[turn] = Message{Role: "user", Content: tok.Sample(r, expanded.SubsequentTurnInputTokens)}
		}
		out[i] = Sample{
			Messages:     []Message{{Role: "user", Content: sessionID + tok.Sample(r, max(1, expanded.FirstTurnInputTokens-8))}},
			FollowUps:    followUps,
			InputTokens:  expanded.FirstTurnInputTokens,
			OutputTokens: expanded.OutputTokens,
		}
	}
	return out, nil
}
