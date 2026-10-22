package messages

import (
	"errors"
	"testing"
)

func TestProductErrorKeepsStableCodeParamsAndEnglishFallback(t *testing.T) {
	err := NewProductError(ErrorAgentUpgradeRequired, "Upgrade the agent.", map[string]any{"minimum_version": "v2"})
	if err.Error() != "Upgrade the agent." || err.Code() != ErrorAgentUpgradeRequired || err.Params()["minimum_version"] != "v2" {
		t.Fatalf("product error = %#v", err)
	}
	if NewProductError("EMPTY", "Fallback", map[string]any{}).Params() != nil {
		t.Fatal("empty params must be omitted from the wire")
	}
}

func TestProductErrorCanPreserveASentinelCause(t *testing.T) {
	cause := errors.New("sentinel")
	err := WrapProductError("WRAPPED", "Fallback", nil, cause)
	if !errors.Is(err, cause) {
		t.Fatal("wrapped product error lost its cause")
	}
}
