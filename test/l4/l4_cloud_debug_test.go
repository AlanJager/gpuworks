//go:build cloudgpu

package l4

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

type debugCredentialProvider struct{ runID string }

func (p debugCredentialProvider) Name() string  { return "fixture" }
func (p debugCredentialProvider) RunID() string { return p.runID }
func (p debugCredentialProvider) Rent(context.Context, cloudprovider.GPUSpec) (*cloudprovider.Instance, error) {
	return nil, nil
}
func (p debugCredentialProvider) Destroy(context.Context, string) error { return nil }
func (p debugCredentialProvider) ListLeaked(context.Context) ([]string, error) {
	return nil, nil
}

func TestSaveCloudDebugCredentialIsPrivateAndUpserts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.json")
	t.Setenv("GPUWORKS_CLOUD_DEBUG_CREDENTIALS_FILE", path)
	provider := debugCredentialProvider{runID: "run/debug"}
	instance := &cloudprovider.Instance{
		InstanceID: "instance-1", PublicIP: "192.0.2.10", SSHPort: 2200,
		Username: "root", Password: "first",
	}
	if got, err := saveCloudDebugCredential(provider, instance); err != nil || got != path {
		t.Fatalf("save path=%q err=%v", got, err)
	}
	instance.Password = "second"
	if _, err := saveCloudDebugCredential(provider, instance); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode=%o, want 600", info.Mode().Perm())
	}
	var records []cloudDebugCredential
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Password != "second" || records[0].RunID != "run/debug" {
		t.Fatalf("credentials records=%+v", records)
	}
}
