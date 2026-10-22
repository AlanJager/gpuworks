package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCUDARuntimeSmokeCommandFailure(t *testing.T) {
	original := cudaSmokeCommand
	t.Cleanup(func() { cudaSmokeCommand = original })
	cudaSmokeCommand = func(context.Context, string, string, ...string) ([]byte, error) {
		return []byte("libcuda missing"), errors.New("exit status 1")
	}
	err := CUDARuntimeSmoke(context.Background())
	if err == nil || !strings.Contains(err.Error(), "libcuda missing") {
		t.Fatalf("smoke error = %v", err)
	}
}

func TestCUDARuntimeSmokeRequiresSuccessMarker(t *testing.T) {
	original := cudaSmokeCommand
	t.Cleanup(func() { cudaSmokeCommand = original })
	cudaSmokeCommand = func(context.Context, string, string, ...string) ([]byte, error) {
		return []byte("unexpected output"), nil
	}
	err := CUDARuntimeSmoke(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no success marker") {
		t.Fatalf("smoke marker error = %v", err)
	}
}
