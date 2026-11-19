package api

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/slurmboot"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// slurmStalledAfter is how long a Slurm entry may wait for its first node
// before the list calls it stalled (dto.SlurmCluster.Stalled): the
// administrator has to run the setup command first, and the health check
// needs a node to register or a reconfigure to fire.
const slurmStalledAfter = 10 * time.Minute

// slurmJoinWindow is how long after the setup was seen run an entry may have
// no node without that meaning anything: the health check runs on each node
// every slurmboot.DefaultIntervalSeconds and the install is spread over half
// of one, so nodes join within one to one and a half intervals; a further
// half interval is allowed for a check that has just run. While it lasts the
// entry waits as "joining" and is not stalled; after it, no node having
// called is a finding.
const slurmJoinWindow = 2 * time.Duration(slurmboot.DefaultIntervalSeconds) * time.Second

// slurmListLag is how long a node that has enrolled may be missing from the
// GPU node list the agents report: an agent reports every slurmReportInterval
// and the entry's list is stored at most once per slurmStateWriteInterval.
const slurmListLag = slurmReportInterval + slurmStateWriteInterval

// Bounds of the settings an entry hands its agents.
const (
	slurmIdleReturnMin     = 1
	slurmIdleReturnMax     = 24 * 60 * 60
	slurmPendingTimeoutMin = 30
	slurmPendingTimeoutMax = 24 * 60 * 60
)

// slurmReportInterval is how often an agent reports the cluster's GPU nodes
// and, with the answer, picks up an edit of its entry's settings. It is what
// the agent runs on (internal/agent/slurm.ReportInterval); the console shows it
// as how long an edit takes to reach the nodes.
const slurmReportInterval = 10 * time.Minute

// slurmMissingNodesLimit bounds the missing nodes a cluster's DTO lists; the
// count is of all of them.
const slurmMissingNodesLimit = 100

// slurmDefaults answers what a new Slurm entry takes for the fields its
// request leaves empty, so the form can show them as hints without a copy.
func (a *apiHandlers) slurmDefaults(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, dto.SlurmDefaults{QuickTunnelURL: a.quickTunnelURL(),
		RunAs: slurmboot.DefaultRunAs, InstallDir: slurmboot.DefaultInstallDir, ScriptDir: slurmboot.DefaultScriptDir,
		IdleReturnSeconds: store.SlurmDefaultIdleReturnSeconds, IdleReturnMinSeconds: slurmIdleReturnMin, IdleReturnMaxSeconds: slurmIdleReturnMax,
		PendingTimeoutSeconds: store.SlurmDefaultPendingTimeoutSeconds, PendingTimeoutMinSeconds: slurmPendingTimeoutMin, PendingTimeoutMaxSeconds: slurmPendingTimeoutMax,
		HealthCheckIntervalSeconds: slurmboot.DefaultIntervalSeconds, StalledAfterSeconds: int(slurmStalledAfter / time.Second),
		ReportIntervalSeconds: int(slurmReportInterval / time.Second), MissingNodesLimit: slurmMissingNodesLimit,
	})
}

// slurmClusterStalled: no node has enrolled for longer than slurmStalledAfter.
func slurmClusterStalled(c store.SlurmCluster, now time.Time) bool {
	return c.SlurmClusterName == "" && c.RevokedAt == nil && now.Sub(c.CreatedAt) > slurmStalledAfter
}

// slurmClusterState is where an entry stands, the one value the console shows
// and filters by: revoked, stalled (no node enrolled for too long), pending
// (no node yet) or active (a node enrolled).
func slurmClusterState(c store.SlurmCluster, stalled bool) string {
	switch {
	case c.RevokedAt != nil:
		return "revoked"
	case stalled:
		return "stalled"
	case c.SlurmClusterName == "":
		return "pending"
	}
	return "active"
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
