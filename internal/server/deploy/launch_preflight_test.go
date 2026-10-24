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
