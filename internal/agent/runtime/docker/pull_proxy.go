package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/pkg/httpproxy"
)

// Docker fetches registry content in the daemon, not the CLI process. A scoped
// choice is safe only when the daemon's observed policy already matches it.
func checkPullProxy(ctx context.Context) error {
	proxy := runtime.PullProxy(ctx)
	if proxy == "" && !runtime.PullDirect(ctx) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Do not log command output: docker info may contain proxy credentials.
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .}}").Output()
	if ctx.Err() != nil {
		return fmt.Errorf("inspect Docker daemon proxy policy: %w", ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("inspect Docker daemon proxy policy: %w", err)
	}
	var info struct {
		HTTPProxy  *string `json:"HttpProxy"`
		HTTPSProxy *string `json:"HttpsProxy"`
		NoProxy    *string `json:"NoProxy"`
	}
	if err := json.Unmarshal(out, &info); err != nil || info.HTTPProxy == nil || info.HTTPSProxy == nil || info.NoProxy == nil {
		return fmt.Errorf("Docker daemon did not report its proxy policy; cannot verify the selected image network path")
	}
	if runtime.PullDirect(ctx) {
		if strings.TrimSpace(*info.HTTPProxy) == "" && strings.TrimSpace(*info.HTTPSProxy) == "" {
			return nil
		}
	} else if strings.TrimSpace(*info.NoProxy) == "" {
		matches := true
		for _, scheme := range []string{"http", "https"} {
			req, _ := http.NewRequest(http.MethodGet, scheme+"://registry.example/", nil)
			selected, selectedErr := httpproxy.Proxy(req, httpproxy.Config{HTTPProxy: proxy, HTTPSProxy: proxy}, false)
			daemonProxy := *info.HTTPProxy
			if scheme == "https" {
				daemonProxy = *info.HTTPSProxy
			}
			if strings.TrimSpace(daemonProxy) == "" {
				matches = false
				break
			}
			observed, observedErr := httpproxy.Proxy(req, httpproxy.Config{HTTPProxy: daemonProxy, HTTPSProxy: daemonProxy}, false)
			if selectedErr != nil || observedErr != nil || selected == nil || observed == nil {
				matches = false
				break
			}
			// Docker masks userinfo, so authenticated proxy equivalence cannot
			// be established from daemon info. Fail closed even for mask literals.
			if selected.User != nil || observed.User != nil {
				return fmt.Errorf("Docker daemon masks proxy credentials; cannot verify the selected authenticated proxy; use a runtime that supports per-pull proxy choices")
			}
			if selected.String() != observed.String() {
				matches = false
				break
			}
		}
		if matches {
			return nil
		}
	}
	return fmt.Errorf("Docker daemon proxy policy does not match the selected image network path; configure the Docker daemon to match or use a runtime that supports per-pull proxy choices")
}
