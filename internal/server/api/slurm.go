package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/slurmboot"
	"gpuworks.ai/gpuworks/internal/server/slurmcluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Slurm clusters join like Kubernetes ones: an administrator creates the
// entry here, runs the setup command it renders on the cluster's controller,
// and the health check that command installs puts an agent on every GPU node
// (see the slurmboot package for the scripts). Each agent trades the entry's
// enrollment token for a node token of its own (POST /slurm/enroll).

func slurmInvalid(w http.ResponseWriter, field, reason string) {
	writeProductError(w, http.StatusBadRequest, ErrorSlurmClusterInvalid, field+": "+reason, map[string]any{"field": field, "reason": reason})
}

// slurmParams are the script parameters of an entry.
func slurmParams(c store.SlurmCluster) slurmboot.Params {
	return slurmboot.Params{ServerURL: c.ServerURL, Token: c.Token, RunAs: c.RunAs, InstallDir: c.InstallDir, ScriptDir: c.ScriptDir}
}

// slurmParamFields maps a script parameter to the request field it came from.
var slurmParamFields = map[string]string{
	"ServerURL": "server_url", "RunAs": "run_as", "InstallDir": "install_dir", "ScriptDir": "script_dir", "Token": "token",
}

var slurmAccountPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// slurmValid checks an entry's parameters, creating or editing it, and
// answers the request itself when one is wrong.
func slurmValid(w http.ResponseWriter, c store.SlurmCluster) bool {
	if c.Name == "" || len(c.Name) > 128 {
		slurmInvalid(w, "name", "must be 1 to 128 bytes")
		return false
	}
	probe := slurmParams(c)
	probe.Token = "x"
	if err := probe.Validate(); err != nil {
		var field *slurmboot.FieldError
		if errors.As(err, &field) {
			slurmInvalid(w, slurmParamFields[field.Field], field.Reason)
		} else {
			slurmInvalid(w, "server_url", err.Error())
		}
		return false
	}
	if c.AccountUser != "" && !slurmAccountPattern.MatchString(c.AccountUser) {
		slurmInvalid(w, "account_user", "must be a login name: lower case letters, digits, _ and -, starting with a letter or _")
		return false
	}
	if len(c.SbatchArgs) > 512 || strings.IndexFunc(c.SbatchArgs, unicode.IsControl) >= 0 {
		slurmInvalid(w, "sbatch_args", "must be at most 512 characters on one line")
		return false
	}
	if c.IdleReturnSeconds < slurmIdleReturnMin || c.IdleReturnSeconds > slurmIdleReturnMax {
		slurmInvalid(w, "idle_return_seconds", "must be between 1 and 86400")
		return false
	}
	if c.PendingTimeoutSeconds < slurmPendingTimeoutMin || c.PendingTimeoutSeconds > slurmPendingTimeoutMax {
		slurmInvalid(w, "pending_timeout_seconds", "must be between 30 and 86400")
		return false
	}
	return true
}

// slurmClusterCategory is the platform type a cluster connected on its own is
// listed under, and what it connects, as provider.Categories declares them.
func slurmClusterCategory() (string, []dto.PlatformResource) {
	for _, c := range provider.Categories() {
		if c.ClusterKind == provider.ClusterKindSlurm {
			return string(c.ID), platformResources(c.Resources)
		}
	}
	return "", []dto.PlatformResource{}
}

// slurmView is what an entry's DTO needs besides the row: what the server
// derives of it (slurmcluster.View) and what the handlers add from the cloud
// account it was made from.
type slurmView struct {
	slurmcluster.View
	canApply, canRemove bool
	blocked             string
	source              store.Provider // the cloud account of the entry's source
	sharedStorage       *dto.SlurmSharedStorageHint
}

// slurmViews derives the views of rows (slurmcluster.Memory.Views, grouped
// queries however many entries and nodes there are) and adds each entry's
// cloud account, the operations it allows, and the shared storage hint.
func (a *apiHandlers) slurmViews(ctx context.Context, rows []store.SlurmCluster) (map[uint]slurmView, error) {
	out := make(map[uint]slurmView, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	cloudCtx, cancelCloud := context.WithTimeout(ctx, slurmCloudListWait)
	defer cancelCloud()
	cloud := &slurmCloudNodes{a: a, ctx: cloudCtx}
	views, err := a.slurm.Views(a.d.Store, rows, cloud.of)
	if err != nil {
		return nil, err
	}
	accounts := a.slurmSourceAccounts(rows)
	accountTargets := map[uint]*slurmAccountTargets{}
	defer func() {
		for _, at := range accountTargets {
			at.cancel()
		}
	}()
	for _, c := range rows {
		v := slurmView{View: views[c.ID]}
		if c.ProviderID != nil {
			v.source = accounts[*c.ProviderID]
		}
		v.canApply, v.canRemove, v.blocked = a.slurmOperations(ctx, c, accountTargets)
		sharedDir := ""
		if a.d.Providers != nil && c.ProviderID != nil {
			sharedDir = a.d.Providers.HPCSharedModelDir(v.source.Kind)
		}
		if hint, ok := planning.SlurmSharedStorageFor(c, sharedDir, v.Nodes, v.HasSharedStorage); ok {
			v.sharedStorage = &dto.SlurmSharedStorageHint{Name: hint.Name, Type: proto.StorageSharedDir, Path: hint.Path, LabelKey: hint.LabelKey, LabelValue: hint.LabelValue}
		}
		out[c.ID] = v
	}
	return out, nil
}

func slurmClusterDTO(c store.SlurmCluster, v slurmView, admin bool) dto.SlurmCluster {
	out := dto.SlurmCluster{
		ID: c.ID, UUID: c.UUID, Name: c.Name, ServerURL: c.ServerURL, SlurmClusterName: c.SlurmClusterName,
		RunAs: c.RunAs, AccountUser: c.AccountUser, InstallDir: c.InstallDir, ScriptDir: c.ScriptDir,
		SbatchArgs: c.SbatchArgs, IdleReturnSeconds: c.IdleReturnSeconds, PendingTimeoutSeconds: c.PendingTimeoutSeconds,
		Revoked: c.RevokedAt != nil, NodeCount: v.Nodes, OnlineCount: v.Online, ListAsOf: v.ListAsOf, SlurmGPUNodes: v.GPUNodes, MissingCount: v.Missing,
		MissingNodes: make([]dto.SlurmMissingNode, 0, len(v.MissingNodes)), CreatedAt: c.CreatedAt.UTC(),
		Stalled: v.Stalled, State: v.State,
	}
	for _, m := range v.MissingNodes {
		out.MissingNodes = append(out.MissingNodes, dto.SlurmMissingNode{Name: m.Name, Reason: m.Reason, NodeUUID: m.NodeUUID, SlurmState: m.SlurmState, SlurmReason: m.SlurmReason})
	}
	out.Category, out.Resources = slurmClusterCategory()
	if c.ProviderID != nil {
		out.Source = &dto.SlurmClusterSource{ProviderID: *c.ProviderID, ProviderName: v.source.Name, ProviderKind: v.source.Kind, ProviderClusterID: c.ProviderClusterID}
	}
	out.CanApply, out.CanRemove, out.OperationBlocked = v.canApply, v.canRemove, v.blocked
	out.Waiting = slurmWaiting(v.Waiting)
	out.SharedStorageHint = v.sharedStorage
	out.DeleteWithNodesAllowed, out.DeleteWithNodesBlocker = v.DeleteAllowed, v.DeleteBlocker
	if admin {
		if c.RevokedAt == nil {
			out.SetupCommand = slurmboot.SetupCommand(c.ServerURL, c.Token)
			out.CheckCommand = slurmboot.CheckCommand(c.ServerURL, c.Token)
		}
		out.RemoveCommand = slurmboot.RemoveCommand(c.ServerURL, c.Token)
		out.UninstallAgentsCommand = slurmboot.UninstallAgentsCommand(c.ServerURL, c.Token)
	}
	return out
}

// slurmClusterView is one entry's DTO.
func (a *apiHandlers) slurmClusterView(ctx context.Context, c store.SlurmCluster, admin bool) (dto.SlurmCluster, error) {
	views, err := a.slurmViews(ctx, []store.SlurmCluster{c})
	if err != nil {
		return dto.SlurmCluster{}, err
	}
	return slurmClusterDTO(c, views[c.ID], admin), nil
}

func (a *apiHandlers) listSlurmClusters(w http.ResponseWriter, r *http.Request) {
	rows, err := a.d.Store.SlurmClusters()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	claims, _ := a.claimsFromRequest(r)
	// The assistant reads this list as the asking administrator but never
	// gets the commands: they carry the enrollment token.
	admin := claims.Role == "admin" && !isAssistantRead(r)
	views, err := a.slurmViews(r.Context(), rows)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	items := make([]dto.SlurmCluster, 0, len(rows))
	for _, c := range rows {
		items = append(items, slurmClusterDTO(c, views[c.ID], admin))
	}
	writeJSON(w, http.StatusOK, dto.NewList(items))
}

func (a *apiHandlers) createSlurmCluster(w http.ResponseWriter, r *http.Request) {
	var req dto.SlurmClusterRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c := store.SlurmCluster{
		Name:                  strings.TrimSpace(req.Name),
		ServerURL:             strings.TrimRight(strings.TrimSpace(req.ServerURL), "/"),
		RunAs:                 firstNonEmpty(strings.TrimSpace(req.RunAs), store.SlurmDefaultRunAs),
		AccountUser:           strings.TrimSpace(req.AccountUser),
		InstallDir:            firstNonEmpty(strings.TrimSpace(req.InstallDir), store.SlurmDefaultInstallDir),
		ScriptDir:             firstNonEmpty(strings.TrimSpace(req.ScriptDir), store.SlurmDefaultScriptDir),
		SbatchArgs:            strings.TrimSpace(req.SbatchArgs),
		IdleReturnSeconds:     store.SlurmDefaultIdleReturnSeconds,
		PendingTimeoutSeconds: store.SlurmDefaultPendingTimeoutSeconds,
	}
	if req.IdleReturnSeconds != nil {
		c.IdleReturnSeconds = *req.IdleReturnSeconds
	}
	if req.PendingTimeoutSeconds != nil {
		c.PendingTimeoutSeconds = *req.PendingTimeoutSeconds
	}
	if (req.ProviderID == nil) != (strings.TrimSpace(req.ProviderClusterID) == "") {
		slurmInvalid(w, "provider_cluster_id", "give provider_id and provider_cluster_id together")
		return
	}
	if req.ProviderID != nil {
		c.ProviderID, c.ProviderClusterID = req.ProviderID, strings.TrimSpace(req.ProviderClusterID)
		if _, err := a.d.Store.GetProvider(*c.ProviderID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				slurmInvalid(w, "provider_id", "no such cloud account")
				return
			}
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	if !slurmValid(w, c) {
		return
	}
	created, err := a.d.Store.CreateSlurmCluster(c)
	switch {
	case errors.Is(err, store.ErrSlurmClusterNameTaken):
		writeProductError(w, http.StatusConflict, ErrorSlurmClusterNameTaken, "another Slurm cluster is named "+c.Name, map[string]any{"name": c.Name})
		return
	case errors.Is(err, store.ErrSlurmClusterSourceTaken):
		writeProductError(w, http.StatusConflict, ErrorSlurmClusterSourceTaken, err.Error(), map[string]any{"cluster": c.ProviderClusterID})
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.d.Log != nil {
		a.d.Log.Info("slurm cluster created", "cluster", created.Name, "uuid", created.UUID)
	}
	view, err := a.slurmClusterView(r.Context(), *created, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (a *apiHandlers) revokeSlurmCluster(w http.ResponseWriter, r *http.Request) {
	uuid := chi.URLParam(r, "uuid")
	if err := a.d.Store.RevokeSlurmCluster(uuid); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	c, err := a.d.Store.SlurmClusterByUUID(uuid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	view, err := a.slurmClusterView(r.Context(), *c, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// slurmClusterSetup answers the commands an administrator runs on the
// cluster for an entry, and where the scripts they fetch are.
func (a *apiHandlers) slurmClusterSetup(w http.ResponseWriter, r *http.Request) {
	c, err := a.d.Store.SlurmClusterByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.SlurmSetup{
		RemoveCommand:          slurmboot.RemoveCommand(c.ServerURL, c.Token),
		UninstallAgentsCommand: slurmboot.UninstallAgentsCommand(c.ServerURL, c.Token),
		DefaultIntervalSeconds: slurmboot.DefaultIntervalSeconds,
	}
	if c.RevokedAt == nil {
		out.SetupCommand = slurmboot.SetupCommand(c.ServerURL, c.Token)
		out.CheckCommand = slurmboot.CheckCommand(c.ServerURL, c.Token)
		out.SetupURL = slurmScriptURL(*c, "setup.sh")
		out.CheckScriptURL = slurmScriptURL(*c, "check.sh")
	}
	writeJSON(w, http.StatusOK, out)
}

// slurmScriptURL is where a script of the entry is served.
func slurmScriptURL(c store.SlurmCluster, name string) string {
	return strings.TrimRight(c.ServerURL, "/") + "/slurm/" + name + "?token=" + url.QueryEscape(c.Token)
}

// writeSlurmScript serves a script to whoever holds the enrollment token,
// which is what lets `curl <url> | sudo sh` fetch it without a login. The
// token is the script's own secret. A revoked entry still serves them: the
// scripts that take the health check out again must stay reachable.
func (a *apiHandlers) writeSlurmScript(w http.ResponseWriter, r *http.Request, render func(slurmboot.Params) string, fetched func(store.SlurmCluster)) {
	c, err := a.d.Store.SlurmClusterByAnyToken(r.URL.Query().Get("token"))
	if err != nil {
		if errors.Is(err, store.ErrSlurmTokenUnknown) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if fetched != nil {
		fetched(*c)
	}
	a.writeSlurmScriptOf(w, *c, render)
}

func (a *apiHandlers) writeSlurmScriptOf(w http.ResponseWriter, c store.SlurmCluster, render func(slurmboot.Params) string) {
	params := slurmParams(c)
	if err := params.Validate(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(render(params)))
}

// slurmSetupByToken serves the setup script by the entry's token, or, for the
// platform's own run on a cloud cluster's manager node, by a one-time code
// (?once=, slurm_once.go) that works once.
func (a *apiHandlers) slurmSetupByToken(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("once")
	if code == "" {
		// A fetch to check only is not the setup having run (slurmboot.CheckQuery).
		checking := r.URL.Query().Get(slurmboot.CheckQuery) != ""
		a.writeSlurmScript(w, r, slurmboot.SetupScript, func(c store.SlurmCluster) {
			if !checking {
				a.slurm.SetupRan(c.ID)
			}
		})
		return
	}
	id, ok := a.slurm.TakeSetupCode(code)
	if !ok {
		writeErr(w, http.StatusNotFound, store.ErrSlurmTokenUnknown)
		return
	}
	c, err := a.d.Store.SlurmClusterByID(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.writeSlurmScriptOf(w, *c, slurmboot.SetupScript)
}

func (a *apiHandlers) slurmCheckByToken(w http.ResponseWriter, r *http.Request) {
	a.writeSlurmScript(w, r, slurmboot.CheckScript, nil)
}

func slurmAgentSettings(c store.SlurmCluster) dto.SlurmAgentSettings {
	return dto.SlurmAgentSettings{SbatchArgs: c.SbatchArgs, IdleReturnSeconds: c.IdleReturnSeconds,
		PendingTimeoutSeconds: c.PendingTimeoutSeconds, AccountUser: c.AccountUser}
}

// slurmEnroll trades an enrollment token for the calling node's node token.
func (a *apiHandlers) slurmEnroll(w http.ResponseWriter, r *http.Request) {
	var req dto.SlurmEnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	node, cluster, err := a.d.Store.EnrollSlurmNode(req.Token, req.SlurmClusterName, req.NodeName)
	switch {
	case errors.Is(err, store.ErrSlurmTokenUnknown):
		writeProductError(w, http.StatusForbidden, ErrorSlurmEnrollRefused, err.Error(), map[string]any{"reason": proto.SlurmEnrollUnknownToken})
		return
	case errors.Is(err, store.ErrSlurmClusterMismatch):
		writeProductError(w, http.StatusForbidden, ErrorSlurmEnrollRefused, err.Error(), map[string]any{"reason": proto.SlurmEnrollOtherCluster})
		return
	case errors.Is(err, store.ErrSlurmNodeTaken):
		// Two agents on one node would share its cards; the agent stops rather
		// than take another entry's identity.
		var taken *store.SlurmNodeTakenError
		errors.As(err, &taken)
		writeProductError(w, http.StatusForbidden, ErrorSlurmEnrollRefused, err.Error(), map[string]any{"reason": proto.SlurmEnrollNodeTaken, "entry": taken.Entry})
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if a.d.Log != nil {
		a.d.Log.Info("slurm node enrolled", "cluster", cluster.Name, "node", node.SlurmNode, "uuid", node.UUID)
	}
	a.slurm.NodeAsked(cluster.ID)
	writeJSON(w, http.StatusOK, dto.SlurmEnrollResponse{NodeUUID: node.UUID, NodeToken: node.Token, Settings: slurmAgentSettings(*cluster)})
}
