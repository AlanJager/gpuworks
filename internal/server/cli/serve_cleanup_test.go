package cli

import (
	"errors"
	"strings"
	"testing"
)

func TestServeCleanupPreservesRunAndCloseResults(t *testing.T) {
	runErr := errors.New("run failure")
	closeErr := errors.New("close failure")
	for _, tc := range []struct {
		name       string
		run, close error
	}{
		{"clean", nil, nil}, {"close-only", nil, closeErr},
		{"run-only", runErr, nil}, {"both", runErr, closeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			result := closeServerResources(tc.run, func() error { calls++; return tc.close })
			if calls != 1 {
				t.Fatalf("close calls=%d", calls)
			}
			if tc.run != nil && !errors.Is(result, tc.run) {
				t.Fatalf("lost Run error: %v", result)
			}
			if tc.close != nil && (!errors.Is(result, tc.close) || !strings.Contains(result.Error(), "close server resources")) {
				t.Fatalf("lost Close error or stage: %v", result)
			}
			if tc.run == nil && tc.close == nil && result != nil {
				t.Fatalf("clean shutdown failed: %v", result)
			}
		})
	}
}
