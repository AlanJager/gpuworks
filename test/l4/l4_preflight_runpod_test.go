//go:build cloudgpu && runpod

package l4

import "gpuworks.ai/gpuworks/internal/testenv/cloudprovider"

func init() {
	preflightProviderFactories = append(preflightProviderFactories, func(secrets map[string]string) (cloudprovider.CloudProvider, error) {
		if secrets["RUNPOD_API_KEY"] == "" {
			return nil, nil
		}
		return cloudprovider.NewRunpod(secrets["RUNPOD_API_KEY"], cloudTestRunID())
	})
}
