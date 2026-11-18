package provider

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Putting a Slurm cluster entry in place from the platform.
//
// An operator connects a Slurm cluster by running the entry's setup command as
// root on the cluster's controller (internal/server/slurmboot). When the
// cluster belongs to a cloud account whose driver can run a command on the
// cluster's manager node, the platform runs the same setup there itself, and
// first adds the gres.conf lines the cluster's GPU compute nodes lack, so the
// operator neither logs in nor repeats it per node (docs/how-to/connect-slurm-nodes.md).

// SlurmManagerRunner is implemented by drivers that can run a script as root
// on the manager node of a cluster in the account.
type SlurmManagerRunner interface {
	// RunOnSlurmManager runs script on the manager node of the HPC cluster
	// clusterID in region and waits for it. Its errors are the SlurmApply*
	// errors of this package. A script that ran and failed is not an error
	// here: its output and how it ended come back for the caller to read.
	RunOnSlurmManager(ctx context.Context, creds Credentials, region, clusterID, script string, timeout time.Duration) (SlurmManagerRun, error)
}

// SlurmManagerRun is a finished command on the manager node.
type SlurmManagerRun struct {
	// ManagerInstance is the cloud instance the script ran on.
	ManagerInstance string
	Output          string
	// Ended is how the cloud says the command ended when it did not succeed,
	// "" when it did.
	Ended string
}

// slurmNodeName is what Slurm names nodes, as far as the scripts carry them.
var slurmNodeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// SlurmApplyTarget is where an entry's setup goes: the cloud account, the
// HPC cluster and its region. ExpectGPU are the Slurm names of the compute
// nodes the account's last sync listed with a card, which only lets a setup
// say which of them Slurm gives no gpu Gres; the nodes that get a gres.conf
// line are the ones Slurm lists itself (SlurmManagerScript).
type SlurmApplyTarget struct {
	ProviderID uint
	ClusterID  string
	Region     string
	ExpectGPU  []string
}

// ErrSlurmApplyNotApplicable: the entry cannot be applied from the platform.
var ErrSlurmApplyNotApplicable = errors.New("the Slurm cluster cannot be set up by the platform")

// SlurmApplyNotApplicableError is ErrSlurmApplyNotApplicable with why
// (messages.SlurmApplyReason*).
type SlurmApplyNotApplicableError struct{ Reason string }

func (e *SlurmApplyNotApplicableError) Error() string {
	return ErrSlurmApplyNotApplicable.Error() + " (" + e.Reason + ")"
}

func (e *SlurmApplyNotApplicableError) Unwrap() error { return ErrSlurmApplyNotApplicable }

// ErrSlurmApplyManagerUnknown: the cloud did not name the manager node.
var ErrSlurmApplyManagerUnknown = errors.New("the cloud did not say which instance is the cluster's manager node")

// SlurmApplyPermissionError: the account's key may not make a call the setup
// needs; Permission is the action (ehpc:GetCluster...).
type SlurmApplyPermissionError struct {
	Permission string
	Err        error
}

func (e *SlurmApplyPermissionError) Error() string {
	return "the account's key lacks permission " + e.Permission + ": " + e.Err.Error()
}

func (e *SlurmApplyPermissionError) Unwrap() error { return e.Err }

// SlurmApplyFailedError: a step did not finish. Step is one of
// messages.SlurmApplyStep*, Reason a stable word when the script said why
// (setup's precheck reasons, server_unreachable, busy), Output the last lines
// the manager printed (or what the cloud said). For server_unreachable, URL is
// the server address the manager node could not fetch the script from (never
// the one-time code) and Kind why, in the words of SlurmReachHost.Reason (dns,
// connect, timeout, tls, http, no_curl, other).
type SlurmApplyFailedError struct{ Step, Reason, Output, URL, Kind string }

func (e *SlurmApplyFailedError) Error() string {
	if e.Reason == "server_unreachable" && e.URL != "" {
		s := "the Slurm manager node cannot reach the management node address " + e.URL
		if e.Kind != "" {
			s += " (" + e.Kind + ")"
		}
		s += ": the cluster's nodes must reach it too (add a NAT gateway when the cluster's VPC has no public egress, or host gpuworks in the same VPC and give its private address)"
		if e.Output != "" {
			s += ": " + e.Output
		}
		return s
	}
	s := "setting up the Slurm cluster failed at " + e.Step
	if e.Reason != "" {
		s += " (" + e.Reason + ")"
	}
	return s + ": " + e.Output
}

// SlurmApplyTarget finds where the entry made from the cloud account's
// cluster is set up, and whether the platform can do it: the account's
// driver can run commands on the manager node, and the cluster schedules with
// Slurm. The cluster's region and scheduler are what the last sync listed for
// its compute nodes; a cluster no node of which was synced (it has none with
// a GPU yet) is read from the account's cloud listing (CachedHPCClusters), at
// most listingAge old. ExpectGPU are the synced nodes with a card; a node's
// Slurm name is its instance name without the "<cluster>_" prefix E-HPC gives
// it, and a node named otherwise is left out (its name in Slurm is not
// known). A cluster without synced nodes has none, which costs nothing: the
// setup reads the nodes and their cards from Slurm. It answers a
// *SlurmApplyNotApplicableError when the platform cannot.
func (s *Service) SlurmApplyTarget(ctx context.Context, providerID uint, clusterID string, listingAge time.Duration) (SlurmApplyTarget, error) {
	targets, err := s.SlurmApplyTargets(providerID, func() (HPCClusterList, error) {
		return s.CachedHPCClusters(ctx, providerID, listingAge)
	})
	if err != nil {
		return SlurmApplyTarget{}, err
	}
	return targets.For(clusterID)
}

// SlurmApplyTargets answers SlurmApplyTarget for any number of the clusters of
// one account from one read of the account's instances and at most one cloud
// listing, which is taken the first time a cluster needs it.
type SlurmApplyTargets struct {
	providerID uint
	blocked    string // why the account cannot run the setup at all, "" when it can
	canList    bool
	synced     map[string]*syncedHPCCluster
	listing    func() (HPCClusterList, error)
	listed     bool
	list       HPCClusterList
	listErr    error
}

// syncedHPCCluster is what the account's synced instances say about a cluster.
type syncedHPCCluster struct {
	region, scheduler string
	expectGPU         []string
	gpuNodes          int // synced nodes with a card, whatever their names
}

// SlurmApplyTargets reads the account's instances once. listing gives the
// account's cloud listing of its HPC clusters, for the clusters no synced
// instance places; it is called at most once. It answers a
// *SlurmApplyNotApplicableError when the account is gone; one whose driver
// cannot run the setup is answered by For, so GPUNodes still works.
func (s *Service) SlurmApplyTargets(providerID uint, listing func() (HPCClusterList, error)) (*SlurmApplyTargets, error) {
	notApplicable := func(reason string) error { return &SlurmApplyNotApplicableError{Reason: reason} }
	p, err := s.store.GetProvider(providerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notApplicable(messages.SlurmApplyReasonNoProvider)
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ProviderInstances(providerID)
	if err != nil {
		return nil, err
	}
	t := &SlurmApplyTargets{providerID: providerID, canList: s.CanListHPCClusters(p.Kind), synced: map[string]*syncedHPCCluster{}, listing: listing}
	if !s.CanRunSlurmSetup(p.Kind) {
		t.blocked = messages.SlurmApplyReasonProviderUnsupported
	}
	for _, r := range rows {
		if r.HPCClusterID == "" {
			continue
		}
		c := t.synced[r.HPCClusterID]
		if c == nil {
			c = &syncedHPCCluster{}
			t.synced[r.HPCClusterID] = c
		}
		c.region, c.scheduler = r.Region, r.HPCScheduler
		if r.GPUCount > 0 {
			c.gpuNodes++
		}
		name, found := store.HPCNodeName(r.HPCClusterID, r.Name)
		if found && r.GPUCount > 0 && slurmNodeName.MatchString(name) {
			c.expectGPU = append(c.expectGPU, name)
		}
	}
	for _, c := range t.synced {
		sort.Strings(c.expectGPU)
	}
	return t, nil
}

// GPUNodes is how many compute nodes of the cluster the account's last sync
// listed with a card; ok is false when no sync has listed the cluster, so the
// count is not known rather than zero.
func (t *SlurmApplyTargets) GPUNodes(clusterID string) (n int, ok bool) {
	if c := t.synced[clusterID]; c != nil {
		return c.gpuNodes, true
	}
	return 0, false
}

// For is the target of one cluster of the account.
func (t *SlurmApplyTargets) For(clusterID string) (SlurmApplyTarget, error) {
	notApplicable := func(reason string) error { return &SlurmApplyNotApplicableError{Reason: reason} }
	if t.blocked != "" {
		return SlurmApplyTarget{}, notApplicable(t.blocked)
	}
	target := SlurmApplyTarget{ProviderID: t.providerID, ClusterID: clusterID}
	scheduler := ""
	if c := t.synced[clusterID]; c != nil && c.region != "" {
		target.Region, scheduler, target.ExpectGPU = c.region, c.scheduler, c.expectGPU
	}
	// A sync whose E-HPC read failed names the cluster's region but not its
	// scheduler: that is unknown, not "not Slurm", so the listing answers it.
	if (target.Region == "" || scheduler == "") && t.canList {
		if !t.listed {
			t.list, t.listErr = t.listing()
			t.listed = true
		}
		switch {
		case errors.Is(t.listErr, ErrListingPending):
			return SlurmApplyTarget{}, notApplicable(messages.SlurmApplyReasonClusterListingPending)
		case t.listErr != nil || t.list.Access == HPCAccessPermissionMissing:
			return SlurmApplyTarget{}, notApplicable(messages.SlurmApplyReasonClusterListingFailed)
		}
		if c, ok := hpcListingOf(t.list.Clusters, clusterID); ok {
			if target.Region == "" {
				target.Region = c.Region
			}
			scheduler = c.Scheduler
		} else if t.list.Partial() {
			// It may be in a region that could not be listed.
			return SlurmApplyTarget{}, notApplicable(messages.SlurmApplyReasonClusterListingFailed)
		}
	}
	switch {
	case target.Region == "":
		return SlurmApplyTarget{}, notApplicable(messages.SlurmApplyReasonClusterUnknown)
	case scheduler != SchedulerSlurm:
		return SlurmApplyTarget{}, notApplicable(messages.SlurmApplyReasonNotSlurmCluster)
	}
	return target, nil
}

// CanListHPCClusters reports whether the driver of a provider kind lists HPC
// clusters.
func (s *Service) CanListHPCClusters(kind string) bool {
	drv, ok := s.driver(kind)
	return ok && isHPCClusterProvider(drv)
}

// CanRunSlurmSetup reports whether the driver of a provider kind can run the
// setup on a cluster's manager node.
func (s *Service) CanRunSlurmSetup(kind string) bool {
	drv, ok := s.driver(kind)
	_, runs := drv.(SlurmManagerRunner)
	return ok && runs
}

// SlurmApplyTimeout bounds one script on the manager node, which may have to
// copy the check script to every node.
const SlurmApplyTimeout = 5 * time.Minute

// RunSlurmSetup runs the setup (req) on the target cluster's manager node and
// reads what it printed.
func (s *Service) RunSlurmSetup(ctx context.Context, target SlurmApplyTarget, req SlurmApplyRequest) (SlurmApplyResult, error) {
	if err := req.Validate(); err != nil {
		return SlurmApplyResult{}, err
	}
	_, drv, creds, err := s.driverCreds(target.ProviderID)
	if err != nil {
		return SlurmApplyResult{}, err
	}
	runner, ok := drv.(SlurmManagerRunner)
	if !ok {
		return SlurmApplyResult{}, &SlurmApplyNotApplicableError{Reason: messages.SlurmApplyReasonProviderUnsupported}
	}
	// The platform asks the compute nodes about the server's address itself,
	// through the cloud's command channel, while the manager runs its script:
	// the two do not wait for each other.
	askNodes := (req.Mode == SlurmModeSetup || req.Mode == SlurmModeCheck) && req.ServerURL != ""
	if _, ok := drv.(SlurmNodeProber); ok && askNodes {
		req.SkipNodeReach = true
	}
	type nodeAnswer struct {
		runs                 []SlurmNodeRun
		unlisted, permission string
	}
	var answer chan nodeAnswer
	if askNodes {
		probeCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		answer = make(chan nodeAnswer, 1)
		go func() {
			var a nodeAnswer
			a.runs, a.unlisted, a.permission = probeSlurmNodes(probeCtx, drv, creds, target, req.ServerURL)
			answer <- a
		}()
	}
	run, err := runner.RunOnSlurmManager(ctx, creds, target.Region, target.ClusterID, SlurmManagerScript(req), SlurmApplyTimeout)
	if err != nil {
		return SlurmApplyResult{}, err
	}
	res, err := ParseSlurmApply(req.Mode, run.Output, run.Ended)
	if err != nil {
		var failed *SlurmApplyFailedError
		if errors.As(err, &failed) && failed.Reason == "server_unreachable" {
			failed.URL = slurmServerBase(req)
		}
		return SlurmApplyResult{}, err
	}
	res.ManagerInstance = run.ManagerInstance
	if askNodes {
		a := <-answer
		res.SetReach(mergeReach(res.Reach.Hosts, a.runs, a.unlisted, a.permission))
	}
	return res, nil
}

// slurmServerBase is the address of the server the manager node was to fetch
// the setup script from, without the script's path or its one-time code: the
// request's ServerURL, else the setup URL up to /slurm/setup.sh.
func slurmServerBase(req SlurmApplyRequest) string {
	if req.ServerURL != "" {
		return strings.TrimRight(req.ServerURL, "/")
	}
	base, _, _ := strings.Cut(req.SetupURL, "/slurm/setup.sh")
	return strings.TrimRight(base, "/")
}
