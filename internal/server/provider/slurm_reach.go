package provider

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Whether the server's address can be reached from the cluster. The health
// check on every GPU node asks the server at the entry's URL, so an address a
// node cannot reach leaves a cluster that applied cleanly and never gets a
// node. The setup script (internal/server/slurmboot) asks from the controller;
// the platform asks each GPU compute node itself through the cloud's command
// channel (SlurmNodeProber), and the two answers are one list, a host to a
// word.

// The roles and answers of a SlurmReachHost.
const (
	SlurmReachController = "controller"
	SlurmReachNode       = "node"

	SlurmReachOK      = "ok"
	SlurmReachFailed  = "failed"
	SlurmReachUnknown = "unknown"
)

// The reasons a host could not be asked (SlurmReachUnknown), besides the
// setup script's own "not_asked": the node was not running, its Cloud
// Assistant was offline, or the command did not finish.
const (
	SlurmReachNotAsked         = "not_asked"
	SlurmReachNodeNotRunning   = "node_not_running"
	SlurmReachAssistantOffline = "assistant_offline"
	SlurmReachCommandFailed    = "command_failed"
)

// SlurmReachHost is what one host said about reaching the server.
type SlurmReachHost struct {
	Role string
	// Name is the host's name in the cluster (the controller's hostname, a
	// compute node's Slurm name).
	Name   string
	Status string
	// Reason says why it failed (dns, connect, timeout, tls, http, no_curl,
	// other) or could not be asked (SlurmReach* above); "" for ok.
	Reason string
}

// SlurmReach is the answer for a whole cluster.
type SlurmReach struct {
	Hosts []SlurmReachHost
	// NodesUnlisted is why the platform could not list the compute nodes to
	// ask them ("" when it did, or was not to): "permission_missing" (with
	// NodesPermission) or "failed".
	NodesUnlisted   string
	NodesPermission string
}

// Failed are the hosts that could not reach the server.
func (r SlurmReach) Failed() []SlurmReachHost {
	var out []SlurmReachHost
	for _, h := range r.Hosts {
		if h.Status == SlurmReachFailed {
			out = append(out, h)
		}
	}
	return out
}

// Asked is how many hosts gave an answer, either way.
func (r SlurmReach) Asked() int {
	n := 0
	for _, h := range r.Hosts {
		if h.Status != SlurmReachUnknown {
			n++
		}
	}
	return n
}

// slurmReachReason words a curl exit status; the setup script's
// reach_reason says the same words.
func slurmReachReason(rc int) string {
	switch rc {
	case 0:
		return ""
	case 127:
		return "no_curl"
	case 6:
		return "dns"
	case 7:
		return "connect"
	case 28:
		return "timeout"
	case 22:
		return "http"
	case 35, 51, 53, 54, 58, 59, 60, 66, 77, 80, 82, 83, 90, 91:
		return "tls"
	}
	return "other"
}

// SlurmNodeProber is implemented by drivers that can run a script as root on
// the GPU compute nodes of a cluster, which the cloud lists itself.
type SlurmNodeProber interface {
	// RunOnSlurmNodes runs script on every GPU compute node of the HPC
	// cluster clusterID in region, a few at a time and within timeout in all,
	// and answers what each printed. A node that could not be asked says why
	// in NotAsked. An error means the nodes could not be listed (a
	// *SlurmApplyPermissionError when the key may not).
	RunOnSlurmNodes(ctx context.Context, creds Credentials, region, clusterID, script string, timeout time.Duration) ([]SlurmNodeRun, error)
}

// SlurmNodeRun is what running the script on one compute node came to.
type SlurmNodeRun struct {
	// Name is the node's name (its hostname in the cluster).
	Name       string
	InstanceID string
	Output     string
	// NotAsked is "" when the script ran to its end, else one of
	// SlurmReachNodeNotRunning, SlurmReachAssistantOffline or
	// SlurmReachCommandFailed.
	NotAsked string
}

// SlurmNodeProbeTimeout bounds asking the compute nodes: a node that has not
// answered by then is not asked.
const SlurmNodeProbeTimeout = 90 * time.Second

// slurmReachProbe is run as root on a compute node: it asks the server's
// /healthz and prints GWKS_REACH rc=<curl's exit status>, 127 for no curl.
const slurmReachProbe = `exec 2>&1
url=@URL@
if ! command -v curl >/dev/null 2>&1; then
  echo "GWKS_REACH rc=127"
  exit 0
fi
curl -fsS --max-time 10 -o /dev/null "$url" >/dev/null 2>&1
echo "GWKS_REACH rc=$?"
exit 0
`

// SlurmReachProbeScript is the script that asks the server's /healthz from a
// compute node. serverURL must be a plain http(s) base URL.
func SlurmReachProbeScript(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(serverURL, "\"'\\\n\r $`") {
		return "", errors.New("the server URL is not a plain http(s) URL")
	}
	return strings.Replace(slurmReachProbe, "@URL@", shellSingleQuote(strings.TrimRight(serverURL, "/")+"/healthz"), 1), nil
}

// reachOfNodeRun reads one node's run as a host: the rc the script printed,
// or why it could not be asked.
func reachOfNodeRun(run SlurmNodeRun) SlurmReachHost {
	host := SlurmReachHost{Role: SlurmReachNode, Name: run.Name}
	if run.NotAsked != "" {
		host.Status, host.Reason = SlurmReachUnknown, run.NotAsked
		return host
	}
	for _, line := range strings.Split(run.Output, "\n") {
		kv, ok := markerLine(line, "GWKS_REACH")
		if !ok {
			continue
		}
		rc, err := strconv.Atoi(kv["rc"])
		if err != nil {
			break
		}
		if rc == 0 {
			host.Status = SlurmReachOK
		} else {
			host.Status, host.Reason = SlurmReachFailed, slurmReachReason(rc)
		}
		return host
	}
	host.Status, host.Reason = SlurmReachUnknown, SlurmReachCommandFailed
	return host
}

// hostKey is how two answers about one host meet: the name before its
// domain, in lower case.
func hostKey(role, name string) string {
	short, _, _ := strings.Cut(strings.ToLower(name), ".")
	return role + "/" + short
}

// rank orders answers about one host: a definite failure outranks a success,
// which outranks not having been asked.
func rank(status string) int {
	switch status {
	case SlurmReachFailed:
		return 2
	case SlurmReachOK:
		return 1
	}
	return 0
}

// mergeReach is the hosts the setup script asked and the nodes the platform
// asked, one entry per host: where both answered, a failure is kept over a
// success, and any answer over none. The controller first, then the nodes by
// name.
func mergeReach(script []SlurmReachHost, runs []SlurmNodeRun, unlisted, permission string) SlurmReach {
	byKey := map[string]SlurmReachHost{}
	add := func(h SlurmReachHost) {
		k := hostKey(h.Role, h.Name)
		if old, ok := byKey[k]; !ok || rank(h.Status) > rank(old.Status) {
			byKey[k] = h
		}
	}
	for _, h := range script {
		add(h)
	}
	for _, r := range runs {
		add(reachOfNodeRun(r))
	}
	out := SlurmReach{NodesUnlisted: unlisted, NodesPermission: permission}
	for _, h := range byKey {
		out.Hosts = append(out.Hosts, h)
	}
	sort.Slice(out.Hosts, func(i, j int) bool {
		a, b := out.Hosts[i], out.Hosts[j]
		if a.Role != b.Role {
			return a.Role == SlurmReachController
		}
		return a.Name < b.Name
	})
	return out
}

// probeSlurmNodes asks the compute nodes of the target cluster, within
// SlurmNodeProbeTimeout. It answers the runs, and when the nodes could not be
// listed why. A driver that cannot ask nodes, or a request that carries no
// server URL, asks none.
func probeSlurmNodes(ctx context.Context, drv any, creds Credentials, target SlurmApplyTarget, serverURL string) (runs []SlurmNodeRun, unlisted, permission string) {
	prober, ok := drv.(SlurmNodeProber)
	if !ok || serverURL == "" {
		return nil, "", ""
	}
	script, err := SlurmReachProbeScript(serverURL)
	if err != nil {
		return nil, "failed", ""
	}
	ctx, cancel := context.WithTimeout(ctx, SlurmNodeProbeTimeout)
	defer cancel()
	runs, err = prober.RunOnSlurmNodes(ctx, creds, target.Region, target.ClusterID, script, SlurmNodeProbeTimeout)
	var denied *SlurmApplyPermissionError
	switch {
	case errors.As(err, &denied):
		return nil, "permission_missing", denied.Permission
	case err != nil:
		return nil, "failed", ""
	}
	return runs, "", ""
}

// SlurmNodeLister is implemented by drivers whose cloud lists the compute
// nodes of an HPC cluster itself.
type SlurmNodeLister interface {
	// SlurmNodeNames lists the names (hostnames in the cluster) of every
	// compute node the cloud has in the HPC cluster clusterID in region,
	// whatever their state. An error means the list is not known whole.
	SlurmNodeNames(ctx context.Context, creds Credentials, region, clusterID string) ([]string, error)
}

// SlurmClusterNodeNames lists the compute nodes the cloud has in the
// account's cluster, for the sweep that removes the nodes which left it: an
// entry whose agents are all gone reports no list of its own, so the cloud's
// is the one that can tell. The cluster's region comes as SlurmApplyTarget
// finds it, from a cloud listing at most listingAge old. It answers
// ErrUnsupported when the account's cloud cannot list the nodes.
func (s *Service) SlurmClusterNodeNames(ctx context.Context, providerID uint, clusterID string, listingAge time.Duration) ([]string, error) {
	target, err := s.SlurmApplyTarget(ctx, providerID, clusterID, listingAge)
	if err != nil {
		return nil, err
	}
	_, drv, creds, err := s.driverCreds(providerID)
	if err != nil {
		return nil, err
	}
	lister, ok := drv.(SlurmNodeLister)
	if !ok {
		return nil, ErrUnsupported
	}
	return lister.SlurmNodeNames(ctx, creds, target.Region, target.ClusterID)
}
