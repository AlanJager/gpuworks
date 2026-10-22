//go:build cloudgpu && compshare

package l4

import "gpuworks.ai/gpuworks/internal/testenv/cloudprovider"

func init() {
	preflightProviderFactories = append(preflightProviderFactories, func(secrets map[string]string) (cloudprovider.CloudProvider, error) {
		if secrets["COMPSHARE_ACCESS_KEY"] == "" || secrets["COMPSHARE_SECRET_KEY"] == "" {
			return nil, nil
		}
		return cloudprovider.NewCompshare(secrets["COMPSHARE_ACCESS_KEY"], secrets["COMPSHARE_SECRET_KEY"], compshareImageID(secrets), "preflight-cleanup")
	})
}
