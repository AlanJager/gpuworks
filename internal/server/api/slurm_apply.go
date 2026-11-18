package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An entry made from a cloud account's Slurm cluster can be put in place by
// the platform: it runs the entry's setup on the cluster's manager node through
// the account's command channel (Cloud Assistant for Alibaba Cloud E-HPC),
// after giving the GPU compute nodes that lack one a gres.conf line
// (provider.SlurmManagerScript). Removing it is the same in two steps.

// slurmApplyTimeout bounds one call: describing the cluster, Cloud Assistant
// reaching the manager node, the script (provider.SlurmApplyTimeout) and
// polling for its result.
const slurmApplyTimeout = provider.SlurmApplyTimeout + 90*time.Second

// slurmSourceAccounts reads the cloud accounts the given entries were made
// from, by id; none is read when no entry has a source.
func (a *apiHandlers) slurmSourceAccounts(rows []store.SlurmCluster) map[uint]store.Provider {
	out := map[uint]store.Provider{}
	for _, c := range rows {
		if c.ProviderID == nil {
			continue
		}
		list, err := a.d.Store.ListProviders()
		if err != nil {
			return out
		}
		for _, p := range list {
			out[p.ID] = p
		}
		break
	}
	return out
}

// slurmCloudListAge is how old the cloud's cluster listing may be when the
// entries' list consults it for a cluster none of whose nodes was synced. The
// list is polled by every open page, so it must not ask the cloud on each
// poll; slurmCloudListWait bounds what one request waits for the cloud. The
// read itself is the provider service's, not the request's (it goes on after
// a request stopped waiting, once per account): a request that stops waiting
// gets the last listing it has, or the entry says the listing is not read yet
// (cluster_listing_pending). A var so a test can shorten the wait.
const slurmCloudListAge = time.Minute

var slurmCloudListWait = 5 * time.Second

// slurmApplyTarget says whether the platform can set the entry up itself, and
// where. A provider.SlurmApplyNotApplicableError says why not. listingAge is
// how old the cloud's listing of the account's clusters may be when the
// entry's cluster has no synced node to say where it is.
func (a *apiHandlers) slurmApplyTarget(ctx context.Context, c store.SlurmCluster, listingAge time.Duration) (provider.SlurmApplyTarget, error) {
	notApplicable := func(reason string) error { return &provider.SlurmApplyNotApplicableError{Reason: reason} }
	switch {
	case c.ProviderID == nil:
		return provider.SlurmApplyTarget{}, notApplicable(messages.SlurmApplyReasonNoSource)
	case a.d.Providers == nil:
		return provider.SlurmApplyTarget{}, notApplicable(messages.SlurmApplyReasonNoProvider)
	}
	return a.d.Providers.SlurmApplyTarget(ctx, *c.ProviderID, c.ProviderClusterID, listingAge)
}

// slurmOperations is dto.SlurmCluster.CanApply, CanRemove and
// OperationBlocked: whether the platform can set the entry up and take it out
// of its cluster, and why not. A revoked entry takes no nodes, so it is not
// applied, but its health check can still be removed.
//
// accounts holds, for one listing of entries, each cloud account's targets
// (provider.SlurmApplyTargets): one read of its instances and at most one of
// its cloud listing answer every entry made from it, so a page of entries
// costs the same whatever their number.
func (a *apiHandlers) slurmOperations(ctx context.Context, c store.SlurmCluster, accounts map[uint]*slurmAccountTargets) (canApply, canRemove bool, blocked string) {
	var err error
	if c.ProviderID == nil || a.d.Providers == nil {
		_, err = a.slurmApplyTarget(ctx, c, slurmCloudListAge)
	} else {
		at := accounts[*c.ProviderID]
		if at == nil {
			at = &slurmAccountTargets{}
			providerID := *c.ProviderID
			// The cloud listing is waited for once per account, not once
			// per entry.
			listCtx, cancel := context.WithTimeout(ctx, slurmCloudListWait)
			at.cancel = cancel
			at.targets, at.err = a.d.Providers.SlurmApplyTargets(providerID, func() (provider.HPCClusterList, error) {
				return a.d.Providers.CachedHPCClusters(listCtx, providerID, slurmCloudListAge)
			})
			accounts[providerID] = at
		}
		if at.err != nil {
			err = at.err
		} else {
			_, err = at.targets.For(c.ProviderClusterID)
		}
	}
	var notApplicable *provider.SlurmApplyNotApplicableError
	switch {
	case errors.As(err, &notApplicable):
		return false, false, notApplicable.Reason
	case err != nil:
		return false, false, ""
	case c.RevokedAt != nil:
		return false, true, messages.SlurmApplyReasonRevoked
	}
	return true, true, ""
}

// slurmAccountTargets is one cloud account's apply targets for a listing of
// entries; cancel ends the wait for its cloud listing.
type slurmAccountTargets struct {
	targets *provider.SlurmApplyTargets
	err     error
	cancel  context.CancelFunc
}

// slurmOnceSetupURL is the URL the manager node fetches the entry's setup
// script from: a one-time code instead of the entry's token, so the command
// the cloud keeps a record of holds no long-lived secret (slurm_once.go).
// done drops the code when the run ended without spending it.
func (a *apiHandlers) slurmOnceSetupURL(c store.SlurmCluster) (string, func(), error) {
	code, err := a.slurmOnce.issue(c.ID)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimRight(c.ServerURL, "/") + "/slurm/setup.sh?once=" + code, func() { a.slurmOnce.discard(code) }, nil
}

// slurmApplyOutcome is what the manager node did, as the API shows it.
func slurmApplyOutcome(res provider.SlurmApplyResult, cluster dto.SlurmCluster) dto.SlurmApplyOutcome {
	out := dto.SlurmApplyOutcome{
		Status: "ok", ManagerInstance: res.ManagerInstance, Steps: make([]dto.SlurmApplyStep, 0, len(res.Steps)),
		Gres:       dto.SlurmApplyGres{Added: nonNil(res.Added), Present: nonNil(res.Present), NoGPUGres: nonNil(res.NoGPUGres)},
		Distribute: dto.SlurmApplyDistribute{Shared: res.Shared, OK: res.DistributeOK, Failed: nonNil(res.DistributeFailed)},
		Resumed:    nonNil(res.Resumed), ResumeFailed: nonNil(res.ResumeFailed), Reach: slurmReachDTO(res.Reach), Cluster: cluster,
	}
	for _, s := range res.Steps {
		out.Steps = append(out.Steps, dto.SlurmApplyStep{ID: s.ID, Status: s.Status})
	}
	if res.Partial() {
		out.Status = "partial"
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// slurmEntryForOperation loads the entry of an apply or remove and checks the
// platform can run it, answering the request itself when it cannot. A revoked
// entry can be removed (its scripts stay served for that) but not applied.
func (a *apiHandlers) slurmEntryForOperation(w http.ResponseWriter, r *http.Request, remove bool) (*store.SlurmCluster, provider.SlurmApplyTarget, bool) {
	c, err := a.d.Store.SlurmClusterByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return nil, provider.SlurmApplyTarget{}, false
	}
	if c.RevokedAt != nil && !remove {
		a.writeSlurmApplyError(w, &provider.SlurmApplyNotApplicableError{Reason: messages.SlurmApplyReasonRevoked})
		return nil, provider.SlurmApplyTarget{}, false
	}
	target, err := a.slurmApplyTarget(r.Context(), *c, 0)
	if err != nil {
		a.writeSlurmApplyError(w, err)
		return nil, provider.SlurmApplyTarget{}, false
	}
	return c, target, true
}

// applySlurmCluster answers POST /slurm/clusters/{uuid}/apply. The body says
// dry_run: true runs the check alone and answers what the setup would do
// (slurmApplyCheck), false carries it out. The setup is idempotent: a run
// again gives the compute nodes added since a gres.conf line.
func (a *apiHandlers) applySlurmCluster(w http.ResponseWriter, r *http.Request) {
	var req dto.SlurmApplyRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.DryRun == nil {
		slurmInvalid(w, "dry_run", "say true to check or false to apply")
		return
	}
	c, target, ok := a.slurmEntryForOperation(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), slurmApplyTimeout)
	defer cancel()
	setupURL, done, err := a.slurmOnceSetupURL(*c)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer done()
	mode := provider.SlurmModeSetup
	if *req.DryRun {
		mode = provider.SlurmModeCheck
	}
	res, err := a.d.Providers.RunSlurmSetup(ctx, target, provider.SlurmApplyRequest{Mode: mode, SetupURL: setupURL, ExpectGPU: target.ExpectGPU, ServerURL: c.ServerURL})
	if err != nil {
		a.writeSlurmApplyError(w, err)
		return
	}
	// What the run found of the server's address is what the entry says it
	// waits for, and a setup that ran is a setup the entry has seen.
	a.slurmActivity.reached(c.ID, res.Reach)
	if !*req.DryRun {
		a.slurmActivity.setupRan(c.ID)
	}
	view, err := a.slurmClusterView(r.Context(), *c, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if *req.DryRun {
		writeJSON(w, http.StatusOK, slurmApplyCheck(res, view))
		return
	}
	out := slurmApplyOutcome(res, view)
	if a.d.Log != nil {
		a.d.Log.Info("slurm cluster applied", "cluster", c.Name, "manager", res.ManagerInstance, "status", out.Status,
			"gres_added", len(res.Added), "resumed", len(res.Resumed))
	}
	writeJSON(w, http.StatusOK, out)
}

// removeSlurmCluster answers POST /slurm/clusters/{uuid}/remove: one of the
// two steps of taking the setup out of the cluster again.
func (a *apiHandlers) removeSlurmCluster(w http.ResponseWriter, r *http.Request) {
	var req dto.SlurmRemoveRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	var mode, next string
	switch req.Step {
	case "uninstall_agents":
		mode, next = provider.SlurmModeUninstallAgents, "remove_check"
	case "remove_check":
		mode = provider.SlurmModeRemoveCheck
	default:
		slurmInvalid(w, "step", "must be uninstall_agents or remove_check")
		return
	}
	c, target, ok := a.slurmEntryForOperation(w, r, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), slurmApplyTimeout)
	defer cancel()
	setupURL, done, err := a.slurmOnceSetupURL(*c)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer done()
	res, err := a.d.Providers.RunSlurmSetup(ctx, target, provider.SlurmApplyRequest{Mode: mode, SetupURL: setupURL})
	if err != nil {
		a.writeSlurmApplyError(w, err)
		return
	}
	view, err := a.slurmClusterView(r.Context(), *c, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	status := "ok"
	if res.RemoveNoop {
		status = "noop"
	}
	if a.d.Log != nil {
		a.d.Log.Info("slurm cluster removal step", "cluster", c.Name, "step", req.Step, "manager", res.ManagerInstance, "status", status)
	}
	writeJSON(w, http.StatusOK, dto.SlurmRemoveOutcome{Step: req.Step, Status: status, ManagerInstance: res.ManagerInstance,
		WaitSeconds: res.IntervalSeconds, NextStep: next, Cluster: view})
}

// writeSlurmApplyError maps the setup's errors to their codes. A refusal of
// the request is 409; a step the cloud or the manager node did not complete
// is 424, as every other provider failure is.
func (a *apiHandlers) writeSlurmApplyError(w http.ResponseWriter, err error) {
	var (
		notApplicable *provider.SlurmApplyNotApplicableError
		permission    *provider.SlurmApplyPermissionError
		failed        *provider.SlurmApplyFailedError
	)
	switch {
	case errors.As(err, &notApplicable):
		writeErr(w, http.StatusConflict, wrapProductError(messages.ErrorSlurmApplyNotApplicable, err.Error(), map[string]any{"reason": notApplicable.Reason}, err))
	case errors.Is(err, provider.ErrSlurmApplyManagerUnknown):
		writeErr(w, statusProviderFailed, wrapProductError(messages.ErrorSlurmApplyManagerUnknown, err.Error(), nil, err))
	case errors.As(err, &permission):
		writeErr(w, statusProviderFailed, wrapProductError(messages.ErrorSlurmApplyPermissionMissing, err.Error(), map[string]any{"permission": permission.Permission}, err))
	case errors.As(err, &failed):
		params := map[string]any{"step": failed.Step, "reason": failed.Reason, "output": failed.Output}
		if failed.URL != "" {
			params["url"] = failed.URL
		}
		if failed.Kind != "" {
			params["kind"] = failed.Kind
		}
		writeErr(w, statusProviderFailed, wrapProductError(messages.ErrorSlurmApplyFailed, err.Error(), params, err))
	case errors.Is(err, store.ErrNotFound):
		// The cloud account the entry came from is gone.
		writeErr(w, http.StatusConflict, wrapProductError(messages.ErrorSlurmApplyNotApplicable, err.Error(), map[string]any{"reason": messages.SlurmApplyReasonNoProvider}, err))
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}
