//go:build cloudgpu && autodl

package l4

import "gpuworks.ai/gpuworks/internal/testenv/cloudprovider"

func init() {
	preflightProviderFactories = append(preflightProviderFactories, func(secrets map[string]string) (cloudprovider.CloudProvider, error) {
		if secrets["AUTODL_TOKEN"] == "" {
			return nil, nil
		}
		return cloudprovider.NewAutoDL(secrets["AUTODL_TOKEN"], "preflight-cleanup")
	})
}
