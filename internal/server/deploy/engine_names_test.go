package deploy

import (
	"reflect"
	"testing"

	enginenames "gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// pkg/enginelog cannot import internal, so it carries its own copy of the
// engine names. This test is the seam that keeps the copy honest: an engine
// added to internal/engines without a log dialect (or the reverse) fails here
// rather than silently parsing engine logs with the vLLM fallback table.
//
// Callable is excluded on both sides: it hosts a catalog-owned Serve Adapter
// and emits no engine log of its own.
func TestEngineLogDialectsMatchEngineNames(t *testing.T) {
	want := enginenames.DialectNames()
	got := enginelog.Dialects()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("enginelog.Dialects()=%v, want enginenames.DialectNames()=%v", got, want)
	}
}

// The registry is populated by init(), so the names it reports must match the
// leaf list every other package reads. A registered engine missing from
// internal/engines would leave callers naming it by literal again.
func TestRegisteredEngineNamesMatchLeafPackage(t *testing.T) {
	want := enginenames.Names()
	got := EngineNames()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("EngineNames()=%v, want enginenames.Names()=%v", got, want)
	}
}

// DefaultEngine must actually resolve; a typo would otherwise surface only as
// a failed deployment.
func TestDefaultEngineResolves(t *testing.T) {
	e, err := SelectEngine("")
	if err != nil {
		t.Fatalf("SelectEngine(\"\") error: %v", err)
	}
	if e.Name() != DefaultEngine {
		t.Fatalf("SelectEngine(\"\").Name()=%q, want %q", e.Name(), DefaultEngine)
	}
}
