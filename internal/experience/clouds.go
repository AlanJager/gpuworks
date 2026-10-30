package experience

import (
	"context"
	"os"
	"path/filepath"
	"time"

	serverapp "gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/provider"
)

// The environment's cloud accounts are data only: an Alibaba Cloud account
// with GPU instances for the public cloud, and a ZStack account with GPU
// hosts for the private cloud, as a sync would have listed them. No vendor
// is ever asked; every operation on them (sync, power, install, edit) is
// refused by the guard. The credentials are placeholders.
type cloudAccount struct {
	kind, name string
	creds      provider.Credentials
	instances  []provider.Instance
}

var cloudAccounts = []cloudAccount{
	{kind: "aliyun", name: "阿里云-华东生产",
		creds: provider.Credentials{PublicKey: "LTAI5tExperienceOnly0000", PrivateKey: "experience-only"},
		instances: []provider.Instance{
			ecs("i-bp1f3k9exp0a10x1", "infer-a10-01", "cn-hangzhou", "cn-hangzhou-k", "NVIDIA A10", 1, 9.84, "203.0.113.31", provider.PowerRunning),
			ecs("i-bp1f3k9exp0a10x4", "infer-a10-02", "cn-hangzhou", "cn-hangzhou-k", "NVIDIA A10", 4, 38.12, "203.0.113.32", provider.PowerRunning),
			ecs("i-uf6c2m7exp0v100x8", "train-v100-01", "cn-shanghai", "cn-shanghai-l", "NVIDIA V100-SXM2-32GB", 8, 112.6, "203.0.113.33", provider.PowerStopped),
		}},
	{kind: "zstack", name: "ZStack 私有云",
		creds: provider.Credentials{Endpoint: "http://10.30.0.10:8080/zstack", PublicKey: "experience-only", PrivateKey: "experience-only"},
		instances: []provider.Instance{
			host("8f1c2a0e5b6d4f7a9e3c1b2d4a6f8e01", "zs-gpu-host-01", "NVIDIA A10", 4, "10.30.0.21"),
			host("8f1c2a0e5b6d4f7a9e3c1b2d4a6f8e02", "zs-gpu-host-02", "NVIDIA L40S", 4, "10.30.0.22"),
			host("8f1c2a0e5b6d4f7a9e3c1b2d4a6f8e03", "zs-cpu-host-01", "", 0, "10.30.0.23"),
		}},
}

func ecs(id, name, region, zone, card string, count int, price float64, ip string, power provider.PowerState) provider.Instance {
	return provider.Instance{ExternalID: id, Name: name, GPUModel: card, GPUCount: count, Power: power, PricePerHr: price,
		Region: region, Zone: zone, PublicIP: ip, SSHPort: 22, SSHUser: "root", Kind: "vm"}
}

func host(id, name, card string, count int, ip string) provider.Instance {
	return provider.Instance{ExternalID: id, Name: name, GPUModel: card, GPUCount: count, Power: provider.PowerRunning,
		Region: "zone-1", PublicIP: ip, SSHPort: 22, SSHUser: "root", Kind: "vm"}
}

// seedClouds adds the accounts once.
func seedClouds(_ context.Context, app *serverapp.App, dataDir string) error {
	marker := filepath.Join(dataDir, "seeded-clouds")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	for _, acct := range cloudAccounts {
		if err := app.SeedCloudAccount(acct.kind, acct.name, acct.creds, acct.instances); err != nil {
			return err
		}
	}
	return os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}
