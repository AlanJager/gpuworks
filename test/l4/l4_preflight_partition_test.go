//go:build cloudgpu && partition

package l4

import "gpuworks.ai/gpuworks/internal/testenv/cloudprovider"

func init() {
	preflightProviderFactories = append(preflightProviderFactories, func(secrets map[string]string) (cloudprovider.CloudProvider, error) {
		if secrets["GITEE_API_TOKEN"] == "" || cloudTestRunID() == "" {
			return nil, nil
		}
		zone := secrets["GITEE_ZONE"]
		if zone == "" {
			zone = "gitee"
		}
		image := secrets["GITEE_IMAGE"]
		if image == "" {
			image = "easzlab.io.local:5000/ascend/torch-npu:2.9.0-CANN-8.5.1-py311-openeuler24.03"
		}
		return cloudprovider.NewGitee(secrets["GITEE_API_TOKEN"], zone, image, cloudTestRunID())
	})
}
