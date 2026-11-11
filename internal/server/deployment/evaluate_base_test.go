package deployment

import (
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEvaluationBaseDistinguishesMissingFromStoreFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "evaluate.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Deps{Store: st})
	for _, id := range []uint{0, 98765} {
		base, err := s.evaluationLaunchBase(id)
		if err != nil || base != nil {
			t.Fatalf("missing %d = %+v, %v", id, base, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.evaluationLaunchBase(98765); err == nil {
		t.Fatal("closed store was silently treated as a missing deployment")
	} else if KindOf(internal(err)) != KindInternal {
		t.Fatal("store failure must remain an internal error")
	}
}

func TestEditBaseRetainsHistoricalArgumentsWithoutWeakeningNewValidation(t *testing.T) {
	off := false
	base := &editLaunchBase{
		Quick: deploy.LaunchConfig{EnforceEager: &off},
		Args:  deploy.StoredLaunchConfig{Engine: "sglang", ExtraArgsText: "--kv-cache-dtype future_dtype"},
	}
	request := &dto.DeployRequest{
		QuickParams: map[string]any{"enforce_eager": false},
		ExtraArgs:   base.Args.ExtraArgsText,
	}
	got, err := materializeDeployRequestWithBase("sglang", request, templateSelection{}, nil, "0.5.21", base)
	if err != nil {
		t.Fatal(err)
	}
	if got.Quick.EnforceEager == nil || *got.Quick.EnforceEager {
		t.Fatal("historical argument handling lost the stored eager Off")
	}
	if _, err := materializeDeployRequest("sglang", request, templateSelection{}, nil, "0.5.21"); err == nil {
		t.Fatal("a new request inherited the historical argument allowance")
	}
	request.ExtraArgs = "--kv-cache-dtype fp8"
	if _, err := materializeDeployRequestWithBase("sglang", request, templateSelection{}, nil, "0.5.21", base); err == nil {
		t.Fatal("changed invalid arguments inherited the historical allowance")
	}
}
