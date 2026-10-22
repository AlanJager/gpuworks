package app

import (
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/httpproxy"
)

// httpClientFor builds a download client honoring the node's HTTP(S) proxy.
func httpClientFor(net proto.NetworkConfig) *http.Client {
	if net.HTTPProxy == "" && net.HTTPSProxy == "" {
		return nil // downloader uses its default client
	}
	return &http.Client{
		Timeout:   5 * time.Minute,
		Transport: httpproxy.Transport(httpproxy.Config{HTTPProxy: net.HTTPProxy, HTTPSProxy: net.HTTPSProxy, NoProxy: net.NoProxy}, false),
	}
}
