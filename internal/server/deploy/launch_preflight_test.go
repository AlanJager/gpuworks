package deploy

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func requireLaunchProductError(t *testing.T, err error, code string) *messages.ProductError {
	t.Helper()
	if err == nil {
		t.Fatalf("launch succeeded, want product error %s", code)
	}
	var product *messages.ProductError
	if !errors.As(err, &product) {
		t.Fatalf("launch error = %v, want product error %s", err, code)
	}
	if product.Code() != code {
		t.Fatalf("launch error code = %q, want %q (error %v)", product.Code(), code, err)
	}
	return product
}

// resolveWithWarnings resolves a launch that must succeed and returns the
// warnings it carries.
func resolveWithWarnings(t *testing.T, input ResolveLaunchInput) (ResolvedLaunch, []messages.Warning) {
	t.Helper()
	launch, warnings, err := ResolveLaunchWithWarnings(input)
	if err != nil {
		t.Fatalf("launch must resolve and only warn: %v", err)
	}
	return launch, warnings
}

// requireWarningCodes asserts the exact warning codes, in order.
func requireWarningCodes(t *testing.T, warnings []messages.Warning, codes ...string) {
	t.Helper()
	got := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		got = append(got, warning.Code)
	}
	if len(got) != len(codes) {
		t.Fatalf("warning codes = %v, want %v", got, codes)
	}
	for index := range codes {
		if got[index] != codes[index] {
			t.Fatalf("warning codes = %v, want %v", got, codes)
		}
	}
}
