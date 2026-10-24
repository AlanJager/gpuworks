package assets

import "runtime"

// AscendRunveilCompatibility returns the interposer for the verified affected
// ARM64 TSD library. Other architectures have no qualified binary yet.
func AscendRunveilCompatibility() ([]byte, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		return nil, nil
	}
	return files.ReadFile("bin/ascend-runveil-compat-arm64.so")
}
