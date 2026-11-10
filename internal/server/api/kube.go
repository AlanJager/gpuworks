package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// Kubernetes clusters join as one DaemonSet (docs/adr/0052-kubernetes-nodes.md):
// an administrator creates the
// cluster here, applies the manifest it renders, and every selected node's
// agent trades the cluster's enrollment token for a node token of its own.

// Defaults a new cluster takes for the fields its request leaves empty.
const (
	kubeDefaultNamespace   = "gpuworks-system"
	kubeDefaultVendor      = accelerator.VendorNVIDIA
	kubeDefaultStateDir    = kubemanifest.DefaultStateDir
	kubeDefaultEnginePorts = "40000-42767"
	kubeAgentImageRepo     = "gpuworks/gpuworks-agent"
)

func kubeDefaultAgentImage() string { return kubeAgentImageRepo + ":" + version.Version }

func kubeManifestURL(c store.KubeCluster) string {
	return strings.TrimRight(c.ServerURL, "/") + "/kube/manifest.yaml?token=" + url.QueryEscape(c.Token)
}

func kubeClusterDTO(c store.KubeCluster, nodes int, admin bool, source *dto.KubeClusterSource, privateAPI bool) dto.KubeCluster {
	out := dto.KubeCluster{
		ID: c.ID, UUID: c.UUID, Name: c.Name, Namespace: c.Namespace, ServerURL: c.ServerURL, AgentImage: c.AgentImage,
		Vendor: c.Vendor, NodeSelector: c.NodeSelector, RuntimeClass: c.RuntimeClass, ImagePullSecret: c.ImagePullSecret,
		ContainerdSocket: c.ContainerdSocket, StateDir: c.StateDir, APIServer: c.APIServer,
		EnginePorts: c.EnginePorts, IdleReturnSeconds: c.IdleReturnSeconds, ClusterUID: c.ClusterUID,
		Revoked: c.RevokedAt != nil, NodeCount: nodes, Source: source, CreatedAt: c.CreatedAt.UTC(),
		Stalled: kubeClusterStalled(c, time.Now().UTC()),
	}
	out.CanApply, out.CanRemove, out.OperationBlocked = kubeClusterOperations(c, privateAPI)
	if admin && c.RevokedAt == nil {
		out.ApplyCommand = "kubectl apply -f '" + kubeManifestURL(c) + "'"
	}
	return out
}

func kubeManifestParams(c store.KubeCluster) (kubemanifest.Params, error) {
	sel, err := kubemanifest.ParseNodeSelector(c.NodeSelector)
	if err != nil {
		return kubemanifest.Params{}, err
	}
	return kubemanifest.Params{
		ClusterName: c.Name, Namespace: c.Namespace, ServerURL: c.ServerURL, Token: c.Token,
		AgentImage: c.AgentImage, Vendor: firstNonEmpty(c.Vendor, kubeDefaultVendor), NodeSelector: sel, RuntimeClass: c.RuntimeClass,
		ImagePullSecret: c.ImagePullSecret, ContainerdSocket: c.ContainerdSocket, EnginePorts: c.EnginePorts, StateDir: c.StateDir, APIServer: c.APIServer,
	}, nil
}

func (a *apiHandlers) listKubeClusters(w http.ResponseWriter, r *http.Request) {
	rows, err := a.d.Store.KubeClusters()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	counts, err := a.d.Store.KubeClusterNodeCounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	claims, _ := a.claimsFromRequest(r)
	// The assistant reads this list as the asking administrator but never
	// gets the apply command: it carries the enrollment token.
	admin := claims.Role == "admin" && !isAssistantRead(r)
	items := make([]dto.KubeCluster, 0, len(rows))
	sources := a.kubeClusterSources(rows)
	private := a.kubePrivateAPIClusters(r.Context(), rows)
	for _, c := range rows {
		items = append(items, kubeClusterDTO(c, counts[c.ID], admin, sources[c.ID], private[c.ID]))
	}
	writeJSON(w, http.StatusOK, dto.NewList(items))
}

func kubeInvalid(w http.ResponseWriter, field, reason string) {
	writeProductError(w, http.StatusBadRequest, ErrorKubeClusterInvalid, field+": "+reason, map[string]any{"field": field, "reason": reason})
}

func (a *apiHandlers) createKubeCluster(w http.ResponseWriter, r *http.Request) {
	var req dto.KubeClusterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c := store.KubeCluster{
		Name:              strings.TrimSpace(req.Name),
		Namespace:         firstNonEmpty(strings.TrimSpace(req.Namespace), kubeDefaultNamespace),
		ServerURL:         strings.TrimRight(strings.TrimSpace(req.ServerURL), "/"),
		AgentImage:        firstNonEmpty(strings.TrimSpace(req.AgentImage), kubeDefaultAgentImage()),
		Vendor:            accelerator.NormalizeVendor(firstNonEmpty(strings.TrimSpace(req.Vendor), kubeDefaultVendor)),
		NodeSelector:      strings.TrimSpace(req.NodeSelector),
		RuntimeClass:      strings.TrimSpace(req.RuntimeClass),
		ImagePullSecret:   strings.TrimSpace(req.ImagePullSecret),
		ContainerdSocket:  strings.TrimSpace(req.ContainerdSocket),
		StateDir:          firstNonEmpty(strings.TrimSpace(req.StateDir), kubeDefaultStateDir),
		APIServer:         strings.TrimSpace(req.APIServer),
		EnginePorts:       firstNonEmpty(strings.TrimSpace(req.EnginePorts), kubeDefaultEnginePorts),
		IdleReturnSeconds: store.KubeDefaultIdleReturnSeconds,
	}
	// An empty RuntimeClass is decided on each node: managed clusters make
	// NVIDIA's runtime the default and define no class, k3s defines
	// "nvidia" beside a default runc. Other vendors' devices come through the
	// agent's device plugin and need none (GET /kube/defaults says which).
	if profile, ok := accelerator.KubeProfileFor(c.Vendor); ok && !profile.RuntimeClass {
		c.RuntimeClass = ""
	}
	if req.IdleReturnSeconds != nil {
		c.IdleReturnSeconds = *req.IdleReturnSeconds
	}
	if !a.kubeClusterSource(w, req, &c) {
		return
	}
	if c.Name == "" || len(c.Name) > 128 {
		kubeInvalid(w, "name", "must be 1 to 128 bytes")
		return
	}
	if _, ok := accelerator.KubeProfileFor(c.Vendor); !ok {
		kubeInvalid(w, "vendor", "must be one of "+strings.Join(accelerator.KubeVendors(), ", "))
		return
	}
	if c.IdleReturnSeconds < store.KubeKeepCards {
		kubeInvalid(w, "idle_return_seconds", "must be -1 (keep) or at least 0")
		return
	}
	if c.ImagePullSecret != "" && !kubemanifest.ValidSecretName(c.ImagePullSecret) {
		kubeInvalid(w, "image_pull_secret", "must be the name of a Secret in the namespace")
		return
	}
	if _, err := proto.ParsePortRange(c.EnginePorts); err != nil {
		kubeInvalid(w, "engine_ports", "must be START-END")
		return
	}
	// Render once so every parameter the manifest needs is checked before
	// the cluster exists.
	probe := c
	probe.Token = "x"
	params, err := kubeManifestParams(probe)
	if err == nil {
		_, err = kubemanifest.Render(params)
	}
	if err != nil {
		kubeInvalid(w, "manifest", err.Error())
		return
	}
	if rows, err := a.d.Store.KubeClusters(); err == nil {
		for _, other := range rows {
			if other.Name == c.Name {
				writeProductError(w, http.StatusConflict, ErrorKubeClusterNameTaken, "another cluster is named "+c.Name, map[string]any{"name": c.Name})
				return
			}
		}
	}
	created, err := a.d.Store.CreateKubeCluster(c)
	if errors.Is(err, store.ErrKubeClusterSourceTaken) {
		writeProductError(w, http.StatusConflict, ErrorKubeClusterSourceTaken, err.Error(), map[string]any{"cluster": c.ProviderClusterID})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.d.Log != nil {
		a.d.Log.Info("kubernetes cluster created", "cluster", created.Name, "uuid", created.UUID)
	}
	writeJSON(w, http.StatusCreated, kubeClusterDTO(*created, 0, true, a.kubeClusterSources([]store.KubeCluster{*created})[created.ID], a.kubePrivateAPIClusters(r.Context(), []store.KubeCluster{*created})[created.ID]))
}

func (a *apiHandlers) revokeKubeCluster(w http.ResponseWriter, r *http.Request) {
	uuid := chi.URLParam(r, "uuid")
	if err := a.d.Store.RevokeKubeCluster(uuid); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	c, err := a.d.Store.KubeClusterByUUID(uuid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	counts, _ := a.d.Store.KubeClusterNodeCounts()
	writeJSON(w, http.StatusOK, kubeClusterDTO(*c, counts[c.ID], true, a.kubeClusterSources([]store.KubeCluster{*c})[c.ID], a.kubePrivateAPIClusters(r.Context(), []store.KubeCluster{*c})[c.ID]))
}

func (a *apiHandlers) deleteKubeCluster(w http.ResponseWriter, r *http.Request) {
	switch err := a.d.Store.DeleteKubeCluster(chi.URLParam(r, "uuid")); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrKubeClusterHasNodes):
		writeProductError(w, http.StatusConflict, ErrorKubeClusterHasNodes, "remove the cluster's nodes first", nil)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func writeKubeManifest(w http.ResponseWriter, c store.KubeCluster) {
	params, err := kubeManifestParams(c)
	var body string
	if err == nil {
		body, err = kubemanifest.Render(params)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// kubeClusterManifest serves the manifest to an administrator.
func (a *apiHandlers) kubeClusterManifest(w http.ResponseWriter, r *http.Request) {
	c, err := a.d.Store.KubeClusterByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeKubeManifest(w, *c)
}

// kubeManifestByToken serves the manifest to whoever holds the enrollment
// token, which is what lets `kubectl apply -f <url>` fetch it without a
// login. The token is the manifest's own secret, so it reveals nothing more.
func (a *apiHandlers) kubeManifestByToken(w http.ResponseWriter, r *http.Request) {
	c, err := a.d.Store.KubeClusterByToken(r.URL.Query().Get("token"))
	if err != nil {
		if errors.Is(err, store.ErrKubeTokenUnknown) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeKubeManifest(w, *c)
}

// kubeEnroll trades an enrollment token for the calling node's node token.
func (a *apiHandlers) kubeEnroll(w http.ResponseWriter, r *http.Request) {
	var req dto.KubeEnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	node, cluster, err := a.d.Store.EnrollKubeNode(req.Token, req.ClusterUID, req.NodeName)
	switch {
	case errors.Is(err, store.ErrKubeTokenUnknown):
		writeProductError(w, http.StatusForbidden, ErrorKubeEnrollRefused, err.Error(), map[string]any{"reason": "unknown_token"})
		return
	case errors.Is(err, store.ErrKubeClusterMismatch):
		writeProductError(w, http.StatusForbidden, ErrorKubeEnrollRefused, err.Error(), map[string]any{"reason": "other_cluster"})
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if a.d.Log != nil {
		a.d.Log.Info("kubernetes node enrolled", "cluster", cluster.Name, "node", node.KubeNodeName, "uuid", node.UUID)
	}
	writeJSON(w, http.StatusOK, dto.KubeEnrollResponse{NodeUUID: node.UUID, NodeToken: node.Token, IdleReturnSeconds: cluster.IdleReturnSeconds})
}

func nodeKubeDTO(n *store.Node) *dto.NodeKube {
	if n.KubeClusterID == 0 {
		return nil
	}
	return &dto.NodeKube{ClusterID: n.KubeClusterID, NodeName: n.KubeNodeName, Unschedulable: n.KubeUnschedulable}
}

// upgradeKubeNode upgrades the agents of a Kubernetes node's cluster: the
// node's agent moves its DaemonSet to this server's version, and Kubernetes
// replaces every agent of the cluster in turn.
func (a *apiHandlers) upgradeKubeNode(w http.ResponseWriter, r *http.Request, node *store.Node) {
	raw, err := a.d.CallNode(r.Context(), node.UUID, proto.RPCKubeUpgrade, proto.KubeUpgradeParams{Version: version.Version})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	var result proto.KubeUpgradeResult
	_ = json.Unmarshal(raw, &result)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "rolling", "image": result.Image})
}
