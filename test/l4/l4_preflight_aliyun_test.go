//go:build cloudgpu && aliyun

package l4

import "gpuworks.ai/gpuworks/internal/testenv/cloudprovider"

// Before the suite starts, anything a crashed earlier attempt of this run left
// behind is released. The scan is scoped by GPUWORKS_CLOUD_TEST_RUN_ID.
func init() {
	preflightProviderFactories = append(preflightProviderFactories, func(secrets map[string]string) (cloudprovider.CloudProvider, error) {
		if secrets["ALIYUN_ACCESS_KEY_ID"] == "" || secrets["ALIYUN_ACCESS_KEY_SECRET"] == "" {
			return nil, nil
		}
		return cloudprovider.NewAliyun(cloudprovider.AliyunConfig{
			AccessKeyID: secrets["ALIYUN_ACCESS_KEY_ID"], AccessKeySecret: secrets["ALIYUN_ACCESS_KEY_SECRET"],
			Regions: splitCSV(secrets["ALIYUN_REGIONS"]),
		}, "preflight-cleanup")
	})
}
