package app

import (
	"testing"
)

// TestDesiredStateParsing covers desiredState's own empty/malformed-JSON
// guard.
func TestDesiredStateParsing(t *testing.T) {
	if _, ok := desiredState(""); ok {
		t.Fatal("desiredState(\"\") must report ok=false")
	}
	if _, ok := desiredState("{not json"); ok {
		t.Fatal("desiredState of malformed JSON must report ok=false")
	}
	spec, ok := desiredState(`{"engine":"vllm","headless":true}`)
	if !ok || spec.Engine != "vllm" || !spec.Headless {
		t.Fatalf("desiredState = %+v, %v, want engine=vllm headless=true, ok=true", spec, ok)
	}
}
