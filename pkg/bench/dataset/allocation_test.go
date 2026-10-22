package dataset

import (
	"fmt"
	"testing"
)

func TestGenerateRejectsImpossibleAllocations(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct {
		name   string
		config Config
		count  int
	}{
		{"sample array", Config{InputTokens: 1}, maxInt / 2},
		{"sample override", Config{Kind: "imported", Samples: []Sample{{Messages: []Message{{Role: "user", Content: "hi"}}}}}, maxInt},
		{"prompt multiplication", Config{InputTokens: maxInt/4 + 1}, 1},
		{"prompt address space", Config{InputTokens: int(MaxAllocationBytes / 4)}, 1},
		{"aggregate allocation", Config{InputTokens: 1024}, int(MaxAllocationBytes / 1024)},
		{"agentic mean", Config{Kind: "agentic", MeanInputTokens: maxInt}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Generate(tc.config, tc.count); err == nil {
				t.Fatalf("expected allocation error for %+v count=%d", tc.config, tc.count)
			}
		})
	}
	for _, kind := range []string{"random", "multiturn", "imported"} {
		t.Run(fmt.Sprint("ordinary_", kind), func(t *testing.T) {
			samples, err := Generate(Config{Kind: kind, InputTokens: 8, OutputTokens: 4, Samples: []Sample{{Messages: []Message{{Role: "user", Content: "hi"}}}}}, 2)
			if err != nil || len(samples) != 2 {
				t.Fatalf("samples=%v err=%v", samples, err)
			}
		})
	}
}
