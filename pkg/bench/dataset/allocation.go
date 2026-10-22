package dataset

import (
	"fmt"
	"reflect"
)

// MaxAllocationBytes is an addressability bound, not a physical memory budget.
// Go's runtime/malloc.go uses a 48-bit heap address space on our supported
// linux/{amd64,arm64} and darwin/arm64 targets. On 32-bit builds, the signed
// slice-length bound is smaller. Keep arithmetic below both bounds before make.
const MaxAllocationBytes = min(uint64(^uint(0)>>1), uint64(1<<48))

// ValidateAllocation rejects impossible sample arrays and synthetic prompt
// sizes before generation. It also protects execution of older stored runs.
// Available RAM may still be less than these addressability bounds.
func ValidateAllocation(c Config, n int) error {
	if n < 0 {
		return fmt.Errorf("sample count must be non-negative")
	}
	size := uint64(reflect.TypeFor[Sample]().Size())
	if uint64(n) > MaxAllocationBytes/size {
		return fmt.Errorf("sample count exceeds the supported allocation size")
	}
	if c.Kind != "" && c.Kind != "random" && c.Kind != "agentic" {
		return nil
	}
	tokens := max(1, c.InputTokens)
	turns := 1
	if c.Kind == "agentic" {
		expanded, err := ExpandAgenticConfig(c)
		if err != nil {
			return err
		}
		tokens = expanded.FirstTurnInputTokens + (expanded.Turns-1)*expanded.SubsequentTurnInputTokens
		turns = expanded.Turns
	}
	// randomText retains a string and temporarily allocates a four-byte/token
	// buffer. Account for both plus sample/message metadata without multiplying
	// unchecked integers. Agentic prompts use their derived input lengths.
	if uint64(tokens) > MaxAllocationBytes/8 {
		return fmt.Errorf("input tokens exceed the supported allocation size")
	}
	size += uint64(tokens)*8 + uint64(turns)*uint64(reflect.TypeFor[Message]().Size())
	if size > MaxAllocationBytes || uint64(n) > MaxAllocationBytes/size {
		return fmt.Errorf("generated dataset exceeds the supported allocation size")
	}
	return nil
}
