package api

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/slurmboot"
	"gpuworks.ai/gpuworks/internal/server/slurmcluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Bounds of the settings an entry hands its agents.
const (
	slurmIdleReturnMin     = 1
	slurmIdleReturnMax     = 24 * 60 * 60
	slurmPendingTimeoutMin = 30
	slurmPendingTimeoutMax = 24 * 60 * 60
)

// slurmDefaults answers what a new Slurm entry takes for the fields its
// request leaves empty, so the form can show them as hints without a copy.
func (a *apiHandlers) slurmDefaults(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, dto.SlurmDefaults{QuickTunnelURL: a.quickTunnelURL(),
		RunAs: slurmboot.DefaultRunAs, InstallDir: slurmboot.DefaultInstallDir, ScriptDir: slurmboot.DefaultScriptDir,
		IdleReturnSeconds: store.SlurmDefaultIdleReturnSeconds, IdleReturnMinSeconds: slurmIdleReturnMin, IdleReturnMaxSeconds: slurmIdleReturnMax,
		PendingTimeoutSeconds: store.SlurmDefaultPendingTimeoutSeconds, PendingTimeoutMinSeconds: slurmPendingTimeoutMin, PendingTimeoutMaxSeconds: slurmPendingTimeoutMax,
		HealthCheckIntervalSeconds: slurmboot.DefaultIntervalSeconds, StalledAfterSeconds: int(slurmcluster.StalledAfter / time.Second),
		ReportIntervalSeconds: int(slurmcluster.ReportInterval / time.Second), MissingNodesLimit: slurmcluster.MissingNodesLimit,
	})
}

// quickTunnelSuffix ends the host of every Cloudflare quick tunnel address
// (`gpuworks expose`, provider cloudflare-quick): a random name under it.
const quickTunnelSuffix = ".trycloudflare.com"

// quickTunnelURL is the platform's own temporary public address while a quick
// tunnel serves it, "" when there is none: the connector's last observation
// (the one node connection diagnostics read) is an address under
// quickTunnelSuffix and its state is not disabled or failed. Such an address
// changes whenever the connector is rebuilt, so a Slurm entry's server URL
// must not be it (docs/how-to/expose-public-address.md).
func (a *apiHandlers) quickTunnelURL() string {
	src, ok := a.d.NodeInstaller.(interface {
		ConnectionExposure() *dto.NodeConnectionExposure
	})
	if !ok {
		return ""
	}
	seen := src.ConnectionExposure()
	if seen == nil || seen.State == "disabled" || seen.State == "failed" {
		return ""
	}
	u, err := url.Parse(seen.URL)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), quickTunnelSuffix) {
		return ""
	}
	return strings.TrimRight(seen.URL, "/")
}
