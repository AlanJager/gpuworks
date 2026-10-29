package accelerator

// Kunlun's whole-card contract belongs to the backend, alongside its native
// runtime requirements. Shared callers consume the contract, not the vendor.
var kunlunTargetConstraint = targetConstraint{
	unsupported:     []Feature{FeatureEngineSleep, FeaturePipelineParallel, FeatureDataParallel, FeaturePD},
	explicitDevices: true,
	runtime:         hostRequirements{os: "linux", arch: "amd64", drivers: []string{"docker"}},
}
