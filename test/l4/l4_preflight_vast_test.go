//go:build cloudgpu && vast

package l4

import "gpuworks.ai/gpuworks/internal/testenv/cloudprovider"

func init() {
	preflightProviderFactories = append(preflightProviderFactories, func(secrets map[string]string) (cloudprovider.CloudProvider, error) {
		if secrets["VAST_API_KEY"] == "" {
			return nil, nil
		}
		key, err := vastPrivateKey(secrets)
		if err != nil {
			return nil, err
		}
		if len(key) == 0 {
			return nil, nil
		}
		return cloudprovider.NewVast(secrets["VAST_API_KEY"], "preflight-cleanup", key)
	})
}
