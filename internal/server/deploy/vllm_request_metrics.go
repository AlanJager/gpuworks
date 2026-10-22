package deploy

import "strings"

// vllmRequestMetricsSince is the first vLLM release whose server accepts
// --enable-per-request-metrics; an older one refuses to start on the unknown
// flag.
const vllmRequestMetricsSince = "0.25.0"

// vllmAcceptsRequestMetrics reports whether the switch's default may turn on
// --enable-per-request-metrics: the image is known to run a vLLM release that
// has it, and no argument layer turns the statistics it reads off
// (--disable-log-stats, which vLLM refuses to combine with it). An image whose
// version nobody recorded gets no default: a default must not stop an image
// from starting. A deployer who turns the switch on gets the flag anyway.
func vllmAcceptsRequestMetrics(version string, layers ...[]string) bool {
	if strings.TrimSpace(version) == "" || compareVersion(version, vllmRequestMetricsSince) < 0 {
		return false
	}
	for _, layer := range layers {
		if flagSet(layer)["--disable-log-stats"] {
			return false
		}
	}
	return true
}
