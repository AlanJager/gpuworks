// Package api wires the REST/SSE surface (chi) and maps store entities to DTOs.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/community"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/recovery"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/webterm"
	"gpuworks.ai/gpuworks/internal/version"
)

// Deps are the collaborators the API needs.
// ImageFetchHooks are the two node-side image operations the API needs: start
// fetching an image onto a node, and stop a transfer that is already running.
type ImageFetchHooks struct {
	Pull   func(ctx context.Context, imageID uint, nodeUUID string, fetch imagecatalog.FetchRequest) (*store.ImageNode, error)
	Cancel func(ctx context.Context, imageID uint, nodeUUID string) (*store.ImageNode, error)
	Remove func(ctx context.Context, imageID uint, nodeUUID string) (*store.ImageNode, error)
}

// OutsideSources are what the API reads from outside the installation: the
// community registries the image page lists tags from, and the distribution
// channel the update checks read (ADR 0040). A nil Community uses a shared
// default browser; a nil Channel reads the built-in sources with the built-in
// keys. Which channel is read is the server's setting (ServerSettings).
type OutsideSources struct {
	AllowPrivateHosts []string
	Community         *community.Browser
	Channel           *distribution.Client
}

var (
	defaultCommunityBrowser = community.NewBrowser(nil)
	defaultChannelClient    = &distribution.Client{}
)

func (s OutsideSources) community() *community.Browser {
	if s.Community != nil {
		return s.Community
	}
	return defaultCommunityBrowser
}

func (s OutsideSources) channel() *distribution.Client {
	if s.Channel != nil {
		return s.Channel
	}
	return defaultChannelClient
}

// NodeStreams are the hub's on-demand byte streams from a node's agent. The api
// layer stays unaware of hub and yamux: each method returns the stream (a
// net.Conn) positioned after its header, and the caller closes it.
//
// They share one Deps field rather than one field each — the same grouping
// MonitorService uses — so adding a stream kind costs an interface method
// instead of another entry in the god struct; see scripts/arch-constraints.md's
// api_deps_fields entry.
type NodeStreams interface {
	// OpenLogtail follows a container's logs (ref = instance uuid). ctx bounds
	// a detached data lane's wait for reattachment.
	OpenLogtail(ctx context.Context, nodeUUID, ref string) (net.Conn, error)
	// OpenLargeFileRead reads one bounded file (absolute path on the node). It
	// needs an attached data lane and fails rather than falling back to ctrl
	// or waiting for one to reattach.
	OpenLargeFileRead(ctx context.Context, nodeUUID, path string) (net.Conn, error)
	// UpgradeAgent pushes the agent binary for arch over the node's own
	// connection; pushed=false means the agent cannot take a push and the
	// agent.upgrade RPC (agent pulls /dl/) is used instead.
	UpgradeAgent(nodeUUID, goos, arch string) (pushed bool, err error)
	// DataLaneAttached reports whether a node's data lane is attached now.
	// The storage-files streams need it and never wait for one.
	DataLaneAttached(nodeUUID string) bool
	// UploadStorageFile sends body on a storage.upload stream (hdr names the
	// storage, target, staging, size, extraction) and returns the agent's
	// verdict. The stream is half-closed only when body ends cleanly; a body
	// error or ctx ending resets it. An error is a transport failure.
	UploadStorageFile(ctx context.Context, nodeUUID string, hdr proto.StreamHeader, body io.Reader) (proto.StorageFilesStatus, error)
	// OpenStorageDownload opens a storage.download stream and reads the
	// agent's head. A refusal is a head with Code set and a nil body;
	// otherwise the caller reads the bytes and closes the body.
	OpenStorageDownload(ctx context.Context, nodeUUID string, hdr proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error)
}

type Deps struct {
	Store         *store.Store
	Bus           *bus.Bus
	Log           *slog.Logger
	SessionCount  func() int       // from hub
	AgentConnect  http.HandlerFunc // hub.ServeWS
	InstallSh     http.HandlerFunc // install script
	RecentRejects func() []dto.AgentReject
	UpgradeErrors func() map[string]string
	UIHandler     http.Handler // embedded SPA (may be nil in dev)

	// Hub integration.
	Online          func(uuid string) bool
	KickNode        func(uuid string)
	NodeSync        NodeSync
	CallNode        func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error)
	SyncModel       func(ctx context.Context, modelID uint, nodeUUID string) (*store.ModelFile, error)
	PeerSync        PeerSyncHooks
	CancelModelSync func(ctx context.Context, modelID uint, nodeUUID string) (*store.ModelFile, error)
	// Preparation is model preparation as the hub and the controller hold it.
	Preparation ModelPreparation
	// NodeRuntime reads and restarts a node's container runtime daemon.
	NodeRuntime     NodeRuntimeHooks
	ImageFetch      ImageFetchHooks
	Outside         OutsideSources
	InspectImage    func(ctx context.Context, imageID uint, nodeUUID string) (proto.ImageInspectResult, error)
	ImageExists     func(ctx context.Context, ref, nodeUUID string) (bool, error)
	CatalogPackages *packages.Store
	ModelLogoDir    string
	// FetchModelMeta reads a model's metadata from the first of candidates
	// that serves it (modelmeta.FetchFrom); fetchModelMeta gives the
	// candidates.
	FetchModelMeta  func(ctx context.Context, source, repo, revision, subfolder string, candidates []string) (*modelmeta.Meta, error)
	ServerDialAgent func(ctx context.Context, addr, secret string) error
	NodeStreams     NodeStreams

	// Web terminal. OpenTerminal mirrors NodeStreams.OpenLogtail: the api layer
	// stays unaware of hub and yamux. Terminal owns admission and audit;
	// NodeCaps lets the handler refuse agents that predate webterm with a clear
	// message.
	OpenTerminal  func(nodeUUID, target, ref string, rows, cols uint16, graceSec int) (net.Conn, error)
	NodeCaps      func(nodeUUID string) []string
	Terminal      *webterm.Manager
	ProxyV1       http.Handler // /v1/* data plane
	ProxyModels   http.Handler // /v1/models
	PlaygroundV1  http.Handler // authenticated internal proxy, no platform API key
	NodePrecheck  provision.Prechecker
	NodeInstaller provision.Installer
	Recovery      interface {
		Trigger(context.Context, string) (recovery.Outcome, error)
	}

	// Cloud providers.
	Providers *provider.Service
	// Networks resolves node networks and applies network changes; nil reads
	// every declared network from Store.
	Networks *network.Service
	// Server is the machine-level configuration (server.yaml): listen
	// addresses and restart.
	Server ServerSettings

	// Metrics + logs. Decoupled from the tsdb/logstore packages via callbacks.
	MetricsQuery func(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error)
	// MetricsLatest accepts an optional inclusive upper bound, applied before
	// selecting the latest sample for each series.
	MetricsLatest func(metric string, matchers map[string]string, sinceMS int64, asOfMS ...int64) ([]MetricLatest, error)
	MetricsIngest func(node string, samples []proto.MetricSample)
	LogsSearch    func(filter LogsFilter) (LogsPage, error)
	LogsContext   func(instance, node string, ts, id int64, before, after int) (LogsWindow, error)
	LogsHistogram func(filter LogsFilter, bucketMS int64) ([]LogsBucket, error)
	// Monitor serves /monitor/overview, /monitor/deployments/{id} and
	// /monitor/series. One field of a small interface (rather than three
	// separate callbacks) keeps api_deps_fields at 51 instead of 53; see
	// scripts/arch-constraints.md's api_deps_fields entry for why 51 replaces
	// this struct's previous limit of 50.
	Monitor MonitorService
	// Telemetry is the export of the gateway's request traces to an
	// OpenTelemetry collector (telemetry_export.go); nil answers 404.
	Telemetry TelemetryService

	// UsageReport is the user experience improvement program's daily report
	// (usage_report.go); Notices the console's notice bell. Nil serves an
	// installation that reports nothing and has no notices.
	UsageReport UsageReport
	Notices     Notices

	// Analytics is usage analytics: the data retention every store is
	// kept under (analytics.go). Nil answers 404.
	Analytics Analytics
	// Experience is the experience environment beside the server
	// (experience.go). Nil answers 404: the environment's own console, or a
	// harness.
	Experience Experience

	// AI serves /ai/available, /ai/ask and the LLM-free findings and fit
	// reads. Nil means no AI service is wired: available reports false.
	AI AIService

	// Management-plane authentication. Data-plane /v1 uses API keys separately.
	AuthSigner           *auth.TokenSigner
	AISecretbox          *auth.Secretbox
	BenchmarkArtifactDir string
}

// PeerSyncHooks groups the direct, explicitly sourced model/image operations
// so the API dependency surface remains stable as the peer protocol grows.
type PeerSyncHooks struct {
	RetryImage           func(ctx context.Context, imageID uint, nodeUUID string) (*store.ImageNode, error)
	SyncModelWithOptions func(ctx context.Context, modelID uint, nodeUUID, sourceNodeUUID string, rateLimitKiB int) (*store.ModelFile, error)
	ProbeModel           func(ctx context.Context, modelID uint, targetNodeUUID, sourceNodeUUID string) error
	SyncImageWithRate    func(ctx context.Context, imageID uint, nodeUUID, sourceNodeUUID string, rateLimitKiB int64) (*store.ImageNode, error)
	ProbeImage           func(ctx context.Context, imageID uint, targetNodeUUID, sourceNodeUUID string) error
	// CopyModel copies a model's files between two locations (storage, and
	// node for a local storage); see controller.CopyModelFile.
	CopyModel func(ctx context.Context, modelID uint, src, dst store.FileRef, rateLimitKiB int) (*store.ModelFile, error)
}

// NodeRuntimeHooks is the hub's view of a node's container runtime daemon:
// the proxy it uses as the agent last reported it, and a restart so a changed
// proxy takes effect.
type NodeRuntimeHooks interface {
	RuntimeProxy(uuid string) *proto.RuntimeProxyStatus
	RestartRuntime(ctx context.Context, uuid string) (*proto.RuntimeProxyStatus, error)
}

// Router builds the top-level HTTP handler.
func Router(d Deps, policies ...auth.SessionPolicy) http.Handler {
	policy := auth.SessionPolicy{}.Normalized()
	if len(policies) > 0 {
		policy = policies[0].Normalized()
	}
	a := &apiHandlers{d: d, sessionPolicy: policy, benchmarkExecutions: map[uint]*benchmarkExecution{}}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(a.countUsage)
	a.aiAsks.limit, a.mcpCalls.limit = aiAsksPerUser, mcpCallsPerUser
	r.Use(a.securityHeaders)
	r.NotFound(a.routingError(http.StatusNotFound))
	r.MethodNotAllowed(a.routingError(http.StatusMethodNotAllowed))

	// Agent long-connection endpoint (WS upgrade → yamux). Authentication is
	// performed by the hub's per-node-token handshake, not management JWT.
	r.Get("/api/v1/agent/connect", d.AgentConnect)

	// Install script (curl | sh, with an explicit per-node token).
	if d.InstallSh != nil {
		r.Get("/install.sh", d.InstallSh)
	}

	// Data plane: OpenAI-compatible /v1. ProxyV1 owns API-key auth for every
	// endpoint, including /v1/models.
	if d.ProxyV1 != nil {
		if d.ProxyModels != nil {
			r.Handle("/v1/models", d.ProxyModels)
		}
		r.Handle("/v1/*", d.ProxyV1)
	}

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/login", a.login)
		r.Post("/auth/logout", a.logout)
		// Open to the sign-in page; it reveals nothing (see prefetchUpdates).
		r.Post("/updates/prefetch", a.prefetchUpdates)
		// Open to the sign-in page, which shows the product name and logo.
		r.Get("/branding", a.getBranding)
		r.With(a.managementAuth).Get("/auth/me", a.me)
		r.With(a.managementAuth, sessionOnly).Post("/auth/activity", a.activity)
		// Platform tokens are managed from a login session only, so no token
		// can mint itself a wider one.
		r.Group(func(r chi.Router) {
			r.Use(a.managementAuth, sessionOnly)
			r.Get("/platform-tokens", a.listPlatformTokens)
			r.Post("/platform-tokens", a.createPlatformToken)
			r.Delete("/platform-tokens/{id}", a.deletePlatformToken)
		})
		// MCP serves read-only tools, so a read token may POST to it.
		r.With(a.managementAuth).Post("/mcp", a.mcp)
		r.Group(func(r chi.Router) {
			r.Use(a.managementAuth, readTokensRead)
			r.Use(compressJSON)
			r.Get("/healthz", a.health)
			r.Get("/engines", a.engines)
			r.Get("/nodes", a.listNodes)
			r.Get("/nodes/facets", a.nodeFacets)
			r.Get("/dashboard/summary", a.dashboardSummary)
			r.With(a.managementAdmin).Post("/nodes", a.createNode)
			r.Get("/agent/rejects", a.listRejects)
			r.Get("/agent/upgrade-errors", a.listUpgradeErrors)
			r.With(a.managementAdmin).Post("/nodes/precheck", a.precheckNode)
			r.With(a.managementAdmin).Post("/nodes/install", a.installNode)
			r.Get("/nodes/{uuid}", a.getNode)
			r.Get("/nodes/{uuid}/fs", a.nodeFSList)
			r.Post("/nodes/{uuid}/doctor", a.nodeDoctor)
			r.Patch("/nodes/{uuid}/runtime", a.updateNodeRuntime)
			r.Patch("/nodes/{uuid}/accelerator", a.updateNodeAccelerator)
			r.Post("/nodes/{uuid}/upgrade", a.upgradeNode)
			r.Put("/nodes/{uuid}/labels", a.updateNodeLabels)
			r.Put("/nodes/{uuid}/scheduling", a.setNodeScheduling)
			r.Get("/nodes/{uuid}/replicas", a.nodeReplicas)
			r.Post("/nodes/{uuid}/evict", a.evictNode)
			r.Patch("/nodes/{uuid}/name", a.renameNode)
			r.Get("/nodes/{uuid}/network", a.nodeNetworkRemoved)
			r.Get("/nodes/{uuid}/pull-proxy", a.getNodePullProxy)
			r.Get("/nodes/{uuid}/networks", a.getNodeNetworks)
			r.With(a.managementAdmin).Put("/nodes/{uuid}/management-roles", a.putNodeManagementRoles)
			r.With(a.managementAdmin).Patch("/nodes/management-roles", a.patchManagementRoles)
			r.With(a.managementAdmin).Put("/nodes/{uuid}/network", a.nodeNetworkRemoved)
			r.With(a.managementAdmin).Post("/nodes/{uuid}/runtime/restart", a.restartNodeRuntime)
			r.Get("/ssh-key", a.getPlatformSSHKey)
			r.With(a.managementAdmin).Put("/ssh-key", a.importPlatformSSHKey)
			r.With(a.managementAdmin).Post("/ssh-key/rotate", a.rotatePlatformSSHKey)
			r.Get("/nodes/{uuid}/ssh", a.getNodeSSH)
			r.With(a.managementAdmin).Put("/nodes/{uuid}/ssh", a.putNodeSSH)
			r.With(a.managementAdmin).Delete("/nodes/{uuid}/ssh", a.deleteNodeSSH)
			r.With(a.managementAdmin).Post("/nodes/{uuid}/recovery/probe", a.probeNodeRecovery)
			r.With(a.managementAdmin).Post("/nodes/{uuid}/token/regenerate", a.regenerateNodeToken)
			r.Delete("/nodes/{uuid}", a.deleteNode)
			r.Get("/events", a.events) // SSE stream of change events

			// Models.
			r.Get("/models", a.listModels)
			r.Post("/models", a.createModel)
			r.Post("/models/quick-register", a.quickRegisterModel)
			r.Post("/models/local/preview", a.previewLocalModel)
			r.Post("/models/local", a.registerLocalModel)
			r.Post("/models/preview-url", a.previewURLModel)
			r.Post("/models/register-url", a.registerURLModel)
			r.Get("/models/{id}/logo", a.getModelLogo)
			r.Get("/models/{id}", a.getModel)
			r.With(a.managementAdmin).Delete("/models/{id}", a.deleteModel)
			r.Patch("/models/{id}/inference-config", a.updateModelInferenceConfig)
			r.Post("/models/{id}/metadata/refresh", a.refreshModelMetadata)
			r.Post("/models/{id}/download", a.downloadModel)
			r.Post("/models/{id}/download/cancel", a.cancelModelDownload)
			r.Post("/models/{id}/copy", a.copyModel)
			r.Post("/models/{id}/nodes/{uuid}/cache/retry", a.retryModelCache)
			r.With(a.managementAdmin).Delete("/models/{id}/storages/{storage_id}", a.deleteModelInStorage)
			r.Post("/models/{id}/register-path", a.registerModelPath)
			r.Post("/models/{id}/nodes/{uuid}/sync", a.syncModelToNode)
			r.Post("/models/{id}/nodes/{uuid}/sync/probe", a.probeModelToNode)
			r.Post("/models/{id}/nodes/{uuid}/sync/cancel", a.cancelModelSync)
			r.With(a.managementAdmin).Delete("/models/{id}/nodes/{uuid}", a.evictModelNode)

			// Image catalog and storage policies.
			r.Get("/images", a.listImages)
			r.Get("/images/versions", a.listImageVersions)
			r.Get("/images/engines", a.listImageEngines)
			r.Get("/images/community/repos", a.listCommunityRepos)
			r.Get("/images/community/browse", a.browseCommunityImages)
			r.With(a.managementAdmin).Patch("/images/versions/{id}", a.patchImageVersion)
			r.Get("/images/{id}", a.getImage)
			r.With(a.managementAdmin).Patch("/images/{id}", a.patchImageSeries)
			r.With(a.managementAdmin).Delete("/images/{id}", a.deleteImage)
			r.With(a.managementAdmin).Delete("/image-series/{id}", a.deleteImageSeries)
			r.With(a.managementAdmin).Post("/images/import", a.importImages)
			r.With(a.managementAdmin).Post("/images/sync", a.syncImagesManifest)
			r.With(a.managementAdmin).Post("/images/preview-archive-url", a.previewImageArchiveURL)
			r.Post("/images/{id}/nodes/{uuid}/sync", a.syncImageToNode)
			r.Post("/images/{id}/nodes/{uuid}/sync/probe", a.probeImageToNode)
			r.Post("/images/{id}/nodes/{uuid}/sync/retry", a.retryImageToNode)
			r.Post("/images/{id}/nodes/{uuid}/cancel", a.cancelImageToNode)
			r.With(a.managementAdmin).Delete("/images/{id}/nodes/{uuid}", a.removeImageFromNode)
			r.With(a.managementAdmin).Post("/images/{id}/nodes/{uuid}/inspect", a.inspectImageOnNode)
			r.Get("/storages", a.listStorages)
			r.Get("/storages/usage", a.storageUsage)
			r.Get("/storages/settings", a.getStorageSettings)
			r.With(a.managementAdmin).Patch("/storages/settings", a.patchStorageSettings)
			r.With(a.managementAdmin).Post("/storages", a.createStorage)
			r.With(a.managementAdmin).Post("/storages/probe", a.probeStorage)
			r.Get("/storages/{id}", a.getStorage)
			r.With(a.managementAdmin).Patch("/storages/{id}", a.patchStorage)
			r.With(a.managementAdmin).Patch("/storages/{id}/node-overrides", a.patchStorageNodeOverrides)
			r.With(a.managementAdmin).Post("/storages/{id}/cache/reconcile", a.reconcileStorageCache)
			r.With(a.managementAdmin).Delete("/storages/{id}", a.deleteStorage)
			r.Get("/storages/{id}/control-planes", a.storageControlPlanes)
			r.Get("/storages/{id}/model-files", a.storageModelFiles)
			r.With(a.managementAdmin).Post("/storages/{id}/scan", a.scanStorage)
			r.With(a.managementAdmin).Post("/storages/{id}/import", a.importStorageModels)
			r.With(a.managementAdmin).Delete("/storages/{id}/cache", a.clearStorageCache)
			// Storage files (ADR 0053): browsing included, admin only.
			r.With(a.managementAdmin).Get("/storages/{id}/files", a.listStorageFiles)
			r.With(a.managementAdmin).Delete("/storages/{id}/files", a.deleteStorageFile)
			r.With(a.managementAdmin).Get("/storages/{id}/files/download", a.downloadStorageFile)
			r.With(a.managementAdmin).Put("/storages/{id}/files/upload", a.uploadStorageFile)
			r.With(a.managementAdmin).Post("/storages/{id}/files/mkdir", a.mkdirStorageFile)
			r.With(a.managementAdmin).Post("/storages/{id}/files/rename", a.renameStorageFile)

			// Inference templates and deployments.
			r.Get("/inference-templates", a.listInferenceTemplates)
			r.Get("/inference-templates/index", a.listInferenceTemplateIndex)
			r.Get("/inference-templates/organizations", a.listInferenceTemplateOrganizations)
			r.Get("/inference-templates/chips", a.listTemplateChips)
			r.Get("/inference-templates/my-chips", a.listDetectedTemplateChips)
			r.Get("/inference-templates/schema", a.inferenceTemplateSchema)
			r.Get("/inference-templates/summary", a.summarizeInferenceTemplates)
			r.With(a.managementAdmin).Get("/inference-templates/export/{digest}", a.exportInferenceTemplatePackage)
			r.Get("/inference-templates/model/*", a.getInferenceTemplate)
			r.Get("/inference-templates/yaml", a.inferenceTemplateYAML)
			r.Post("/inference-templates/clone", a.cloneInferenceTemplate)
			r.With(a.managementAdmin).Post("/inference-templates/import", a.importInferenceTemplates)
			r.With(a.managementAdmin).Post("/inference-templates/evaluate", a.evaluateInferenceTemplates)
			r.With(a.managementAdmin).Delete("/inference-templates/spec", a.deleteInferenceTemplateSpec)
			r.With(a.managementAdmin).Post("/inference-templates/sync", a.syncInferenceTemplates)
			r.With(a.managementAdmin).Post("/inference-templates/official", a.uploadOfficialInferenceCatalog)
			r.Get("/deployments", a.listDeployments)
			r.Get("/routing-policies", a.routingPolicies)
			r.Post("/deployments/preflight", a.distributedPreflight)
			r.Post("/deployments", a.createDeployment)
			r.Get("/deployments/{id}", a.getDeployment)
			r.Get("/deployments/{id}/candidates", a.deploymentCandidates)
			r.Get("/deployments/{id}/draft", a.getDeploymentDraft)
			r.Put("/deployments/{id}", a.editDeployment)
			r.Put("/deployments/{id}/requirements", a.setDeploymentRequirements)
			r.Put("/deployments/{id}/routing", a.setRoutingPolicy)
			r.Put("/deployments/{id}/health-check", a.setHealthCheck)
			r.Put("/deployments/{id}/tokenize", a.setTokenize)
			r.Put("/deployments/{id}/dgpu", a.setDeploymentDGPUQuota)
			r.Post("/deployments/{id}/clone", a.cloneDeployment)
			r.Post("/deployments/{id}/stop", a.stopDeployment)
			r.Post("/deployments/{id}/start", a.startDeployment)
			r.Put("/deployments/{id}/pd", a.updatePD)
			r.Put("/deployments/{id}/replicas", a.scaleReplicas)
			r.Post("/deployments/{id}/restart", a.restartDeployment)
			r.Post("/deployments/{id}/units/retry", a.retryUnit)
			r.Post("/deployments/{id}/units/suggest", a.suggestUnit)
			r.Delete("/deployments/{id}", a.deleteDeployment)
			r.Post("/deployments/{id}/sleep", a.sleepDeployment)
			r.Post("/deployments/{id}/wake", a.wakeDeployment)
			r.Put("/deployments/{id}/node-replacement", a.setNodeReplacement)
			r.Put("/deployments/{id}/placement-mode", a.setPlacementMode)
			// A deployment's own timeline, including scheduling outcomes
			// recorded before any instance existed.
			r.Get("/deployments/{id}/events", a.deploymentEvents)
			r.Post("/evaluate", a.evaluate)
			r.Get("/benchmarks", a.listBenchmarks)
			r.Get("/benchmarks/meta", a.getBenchmarkMeta)
			r.Post("/benchmarks", a.createBenchmark)
			r.Post("/benchmarks/probe", a.probeBenchmark)
			r.Post("/benchmarks/models", a.listBenchmarkModels)
			r.Post("/benchmarks/datasets/import", a.importBenchmarkDataset)
			r.Post("/benchmarks/tokenizers/import", a.importBenchmarkTokenizer)
			r.Get("/benchmarks/{id}", a.getBenchmark)
			r.Get("/benchmarks/{id}/curve", a.getBenchmarkCurve)
			r.Get("/benchmarks/{id}/report", a.downloadBenchmarkReport)
			r.Delete("/benchmarks/{id}", a.deleteBenchmark)
			r.Get("/benchmarks/{id}/points/{seq}/detail", a.downloadBenchmarkDetail)
			r.Post("/benchmarks/{id}/cancel", a.cancelBenchmark)

			// Instances.
			r.Get("/instances", a.listInstances)
			r.Get("/instances/{uuid}", a.getInstance)
			r.Get("/instances/{uuid}/launch-facts", a.getInstanceLaunchFacts)
			r.Get("/instances/{uuid}/events", a.instanceEvents)
			r.Get("/instances/{uuid}/logs", a.instanceLogs) // SSE
			r.Post("/instances/{uuid}/restart", a.restartInstance)
			r.Post("/instances/{uuid}/sleep", a.sleepInstance)
			r.Post("/instances/{uuid}/wake", a.wakeInstance)

			// API keys.
			r.Get("/apikeys", a.listAPIKeys)
			r.Post("/apikeys", a.createAPIKey)
			r.Delete("/apikeys/{prefix}", a.deleteAPIKey)
			r.Get("/mcp/status", a.mcpStatus)
			r.With(a.managementAdmin).Put("/mcp/settings", a.putMCPSettings)

			// Cloud providers.
			r.Get("/providers/kinds", a.providerKinds)
			r.Get("/providers", a.listProviders)
			r.Post("/providers", a.addProvider)
			r.With(sessionOnly).Get("/providers/{id}", a.getProvider)
			r.Put("/providers/{id}", a.updateProvider)
			r.Delete("/providers/{id}", a.deleteProvider)
			r.Post("/providers/{id}/sync", a.syncProvider)
			r.Post("/providers/{id}/instances", a.createProviderInstance)
			r.Get("/providers/{id}/instances", a.providerInstances)
			r.Delete("/providers/{id}/instances/{externalID}", a.deleteProviderInstance)
			r.Post("/providers/{id}/instances/{externalID}/power", a.powerProviderInstance)
			r.Post("/providers/{id}/instances/{externalID}/resize", a.resizeProviderInstance)
			r.Post("/providers/{id}/install", a.installAgentOnInstance)

			// Networks.
			r.Get("/networks", a.listNetworks)
			r.With(a.managementAdmin).Post("/networks", a.createNetwork)
			r.Get("/networks/{id}", a.getNetwork)
			r.With(a.managementAdmin).Put("/networks/{id}", a.updateNetwork)
			r.With(a.managementAdmin).Delete("/networks/{id}", a.deleteNetwork)
			r.With(a.managementAdmin).Post("/networks/{id}/attachments", a.createNetworkAttachment)
			r.With(a.managementAdmin).Post("/networks/{id}/attachments/batch", a.attachNetworkBatch)
			r.With(a.managementAdmin).Post("/networks/{id}/hcas", a.editNetworkHCAs)
			r.With(a.managementAdmin).Put("/networks/{id}/attachments/{attachmentID}", a.updateNetworkAttachment)
			r.With(a.managementAdmin).Delete("/networks/{id}/attachments/{attachmentID}", a.deleteNetworkAttachment)
			r.With(a.managementAdmin).Post("/networks/{id}/tests", a.startNetworkTest)
			r.With(a.managementAdmin).Post("/networks/{id}/tests/estimate", a.estimateNetworkTest)

			// Web terminal. Admin-only in both branches: a host terminal is
			// a root shell on a GPU node, and the container branch is not worth a
			// separate per-deployment authorization model yet.
			r.With(a.managementAdmin).Get("/nodes/{uuid}/terminal", a.nodeTerminal)
			r.With(a.managementAdmin).Get("/nodes/{uuid}/terminal/settings", a.terminalSettings)
			r.With(a.managementAdmin).Get("/terminal/sessions", a.terminalSessionsList)

			// Pull settings: where nodes get images and models from.
			r.Get("/pull/source-rules", a.listPullSourceRules)
			r.With(a.managementAdmin).Post("/pull/source-rules", a.createPullSourceRule)
			r.With(a.managementAdmin).Put("/pull/source-rules/order", a.orderPullSourceRules)
			r.With(a.managementAdmin).Post("/pull/source-rules/test", a.testSourceRule)
			r.With(a.managementAdmin).Put("/pull/source-rules/{id}", a.updatePullSourceRule)
			r.With(a.managementAdmin).Delete("/pull/source-rules/{id}", a.deletePullSourceRule)
			r.Get("/pull/metadata-sources", a.metadataSources)
			r.Get("/pull/proxies", a.listPullProxies)
			r.With(a.managementAdmin).Post("/pull/proxies", a.createPullProxy)
			r.With(a.managementAdmin).Put("/pull/proxies/order", a.orderPullProxies)
			r.With(a.managementAdmin).Put("/pull/proxies/{id}", a.updatePullProxy)
			r.With(a.managementAdmin).Delete("/pull/proxies/{id}", a.deletePullProxy)
			r.Get("/pull/sync-groups", a.listSyncGroups)
			r.With(a.managementAdmin).Post("/pull/sync-groups", a.createSyncGroup)
			r.With(a.managementAdmin).Put("/pull/sync-groups/{id}", a.updateSyncGroup)
			r.With(a.managementAdmin).Delete("/pull/sync-groups/{id}", a.deleteSyncGroup)
			r.With(a.managementAdmin).Get("/pull/credentials", a.listCredentials)
			r.With(a.managementAdmin).Post("/pull/credentials", a.createCredential)
			r.With(a.managementAdmin).Put("/pull/credentials/{id}", a.updateCredential)
			r.With(a.managementAdmin).Delete("/pull/credentials/{id}", a.deleteCredential)
			r.With(a.managementAdmin).Post("/pull/credentials/{id}/test", a.testCredential)
			r.With(a.managementAdmin).Get("/pull/registry-credentials", a.registryCredentialsRemoved)
			r.With(a.managementAdmin).Post("/pull/registry-credentials", a.registryCredentialsRemoved)
			r.With(a.managementAdmin).Put("/pull/registry-credentials/{id}", a.registryCredentialsRemoved)
			r.With(a.managementAdmin).Delete("/pull/registry-credentials/{id}", a.registryCredentialsRemoved)
			r.With(a.managementAdmin).Post("/pull/registry-credentials/{id}/test", a.registryCredentialsRemoved)

			// Settings (network/accel presets, etc.).
			r.Get("/settings", a.getSettings)
			r.With(a.managementAdmin).Put("/settings/{key}", a.putSetting)
			r.Get("/telemetry/export", a.getTelemetryExport)
			r.With(a.managementAdmin).Put("/telemetry/export", a.putTelemetryExport)
			r.With(a.managementAdmin).Post("/telemetry/export/test", a.testTelemetryExport)
			r.Get("/security/settings", a.getSecuritySettings)
			r.With(a.managementAdmin).Patch("/security/settings", a.patchSecuritySettings)
			r.With(a.managementAdmin).Patch("/branding", a.patchBranding)
			r.Get("/data-retention", a.getDataRetention)
			r.With(a.managementAdmin).Put("/data-retention", a.putDataRetention)
			r.Get("/data-retention/status", a.getDataRetentionStatus)
			r.Get("/analytics/usage", analyticsRoute(a, Analytics.Usage))
			r.Get("/analytics/quality", analyticsRoute(a, Analytics.Quality))
			r.Get("/analytics/gpus", analyticsRoute(a, Analytics.GPUs))
			r.Get("/analytics/gpu-time", analyticsRoute(a, Analytics.GPUTime))
			r.Get("/analytics/key-usage", a.getKeyUsage)
			r.With(a.managementAdmin).Get("/updates", a.getUpdates)
			r.With(a.managementAdmin).Put("/updates/channel", a.putUpdateChannel)
			r.With(a.managementAdmin).Put("/updates/auto-check", a.putUpdateAutoCheck)
			r.With(a.managementAdmin).Post("/updates/server", a.upgradeServerFromChannel)
			r.With(a.managementAdmin).Post("/updates/server/upload", a.upgradeServerFromUpload)
			r.With(a.managementAdmin).Get("/usage-report", a.getUsageReport)
			r.With(a.managementAdmin).Put("/usage-report/enabled", a.putUsageReportEnabled)
			r.With(a.managementAdmin).Get("/usage-report/preview", a.getUsageReportPreview)
			r.With(a.managementAdmin).Get("/experience", a.getExperience)
			r.With(a.managementAdmin).Put("/experience/enabled", a.putExperienceEnabled)
			r.With(a.managementAdmin).Post("/experience/reset", a.postExperienceReset)
			r.With(a.managementAdmin).Post("/experience/password", a.postExperiencePassword)
			r.Get("/notices", a.getNotices)
			r.Post("/notices/read", a.postNoticesRead)
			r.With(a.managementAdmin).Get("/server/listen", a.getServerListen)
			r.With(a.managementAdmin).Put("/server/listen", a.putServerListen)
			r.With(a.managementAdmin).Post("/server/restart", a.restartServer)

			// Metrics + logs.
			r.Get("/metrics/query", a.metricsQuery)
			r.Get("/logs/search", a.logsSearch)
			r.Get("/logs/context", a.logsContext)
			r.Get("/logs/histogram", a.logsHistogram)
			r.Get("/monitor/overview", a.monitorOverview)
			r.Get("/monitor/deployments/{id}", a.monitorDeployment)
			r.Get("/monitor/deployments/{id}/requests", a.monitorRequests)
			r.Get("/monitor/requests/lookup", a.monitorRequestLookup)
			r.Get("/monitor/distributions", a.monitorDistributions)
			r.Get("/monitor/deployments/{id}/latency", a.monitorLatency)
			r.Get("/monitor/metrics", a.monitorMetrics)
			r.Get("/monitor/series", a.monitorSeries)
			r.Get("/gpus", a.listGPUs)
			r.Get("/dgpus", a.listDGPUs)

			// AI helper.
			r.Get("/ai/configs", a.listAIConfigs)
			r.Post("/ai/configs", a.createAIConfig)
			// The connection test changes nothing, so it is a POST to a verb
			// path rather than a resource write under {id}.
			r.Post("/ai/configs/test", a.testAIConfig)
			r.Patch("/ai/configs/{id}", a.updateAIConfig)
			r.Delete("/ai/configs/{id}", a.deleteAIConfig)
			r.Get("/ai/available", a.aiAvailable)
			r.Post("/ai/ask", a.aiAsk)
			r.Get("/ai/instances/{uuid}/findings", a.aiInstanceFindings)
			r.Get("/ai/models/{id}/fit", a.aiModelFit)

			// Playground stays inside management JWT auth. Its proxy may attach an
			// encrypted external-provider key, so it must never be mounted at /v1.
			r.Get("/playground/models", a.playgroundModels)
			r.Get("/playground/openapi.json", func(w http.ResponseWriter, req *http.Request) {
				if d.PlaygroundV1 == nil {
					http.NotFound(w, req)
					return
				}
				d.PlaygroundV1.ServeHTTP(w, req)
			})
			if d.PlaygroundV1 != nil {
				r.Handle("/playground/v1/*", a.withClaims(d.PlaygroundV1))
			}
		})
	})
	r.Get("/healthz", a.health)

	// SPA fallback for everything else.
	if d.UIHandler != nil {
		r.Handle("/*", d.UIHandler)
	}
	a.mux = r
	return r
}

type apiHandlers struct {
	sessionPolicy       auth.SessionPolicy
	loginThrottle       loginThrottle
	d                   Deps
	gpuCacheMu          sync.Mutex
	gpuCacheAt          time.Time
	gpuCache            *gpuInventoryCache
	gpuCacheGeneration  uint64
	benchmarkMu         sync.Mutex
	benchmarkExecutions map[uint]*benchmarkExecution
	updates             distribution.CheckCache
	// mux is the router the handlers are mounted on, for the assistant's
	// in-process reads (ai_reader.go).
	mux *chi.Mux
	routesState
	// aiAsks and mcpCalls count each user's questions and MCP tool calls
	// in flight (ai.go, mcp.go).
	aiAsks, mcpCalls inFlightLimiter
}

func (a *apiHandlers) health(w http.ResponseWriter, r *http.Request) {
	nodes, _ := a.d.Store.ListNodes()
	sessions := 0
	if a.d.SessionCount != nil {
		sessions = a.d.SessionCount()
	}
	writeJSON(w, http.StatusOK, dto.Health{
		Status:   "ok",
		Version:  version.Version,
		Nodes:    len(nodes),
		Sessions: sessions,
	})
}

func (a *apiHandlers) createNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string `json:"name"`
		ConnectMode      string `json:"connect_mode"`
		ServerDialListen string `json:"server_dial_listen"`
		// HostAddress is where the server reaches the host for server_dial.
		HostAddress string `json:"host_address"`
		ServerURL   string `json:"server_url"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	if !validateNodeName(w, req.Name) {
		return
	}
	serverDialListen := strings.TrimSpace(req.ServerDialListen)
	if serverDialListen == "" {
		serverDialListen = provision.DefaultServerDialListen
	}
	var serverDialAddress string
	if req.ConnectMode == "server_dial" {
		if err := validServerDialListen(serverDialListen); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(req.HostAddress) == "" {
			writeProductError(w, http.StatusBadRequest, ErrorHostAddressRequired, "host_address is required for server_dial", nil)
			return
		}
		serverDialAddress = provision.ServerDialAddress(req.HostAddress, serverDialListen)
		if serverDialAddress == "" {
			writeProductError(w, http.StatusBadRequest, ErrorHostAddressInvalid, fmt.Sprintf("invalid host_address %q", req.HostAddress), map[string]any{"address": req.HostAddress})
			return
		}
	}
	// The address the agent will dial is the caller's to give (the console
	// sends the one its browser uses); the server does not guess its own.
	// An agent the server dials has none.
	serverURL, serverURLErr := installServerURL(req.ServerURL, req.ConnectMode)
	if serverURLErr != nil {
		writeErr(w, http.StatusBadRequest, serverURLErr)
		return
	}
	n, token, err := a.d.Store.CreateNodePending(req.Name, req.ConnectMode)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.d.Store.SetNodeServerURL(n.UUID, serverURL); err != nil {
		_ = a.d.Store.DeleteNode(n.UUID)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.d.Store.SetNodeServerDialListen(n.UUID, serverDialListen); err != nil {
		_ = a.d.Store.DeleteNode(n.UUID)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	n.ServerDialListen = serverDialListen
	n.ServerURL = strings.TrimRight(serverURL, "/")
	cmd := provision.InstallCommand(serverURL, token, req.ConnectMode, serverDialListen)
	if req.ConnectMode == "server_dial" {
		if err := a.d.Store.SetNodeServerDialAddress(n.UUID, serverDialAddress); err != nil {
			_ = a.d.Store.DeleteNode(n.UUID)
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		n.ServerDialAddress = serverDialAddress
	}
	writeJSON(w, http.StatusCreated, map[string]any{"node": toNodeDTO(n), "token": token, "install_command": cmd, "server_url": serverURL, "server_dial_address": n.ServerDialAddress})
}

// installServerURL is the server address an install records for the agent
// to dial: none for an agent the server dials (server_dial), which never
// contacts the server, whatever the caller sent; otherwise the caller's,
// required and validated (normalizeInstallServerURL).
func installServerURL(raw, connectMode string) (string, error) {
	if connectMode == "server_dial" {
		return "", nil
	}
	return normalizeInstallServerURL(raw)
}

// validServerDialListen rejects a listen address a server_dial agent cannot
// listen on before anything is created for it.
func validServerDialListen(listen string) error {
	if provision.ValidServerDialListen(listen) {
		return nil
	}
	return productError(ErrorServerDialListenInvalid, fmt.Sprintf("invalid server_dial_listen %q: use host:port or :port", listen), map[string]any{"address": listen})
}

// validCloudInstallDir accepts the install directory of a cloud install: an
// absolute path other than /, or "" for the default. Some providers hand it
// to install.sh on the instance, where nobody expands "~" for the server to
// record where the agent went.
func validCloudInstallDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	if strings.HasPrefix(dir, "/") && strings.TrimRight(dir, "/") != "" && !strings.ContainsAny(dir, "\x00\n\r") {
		return nil
	}
	return productError(ErrorInstallDirInvalid, fmt.Sprintf("invalid install_dir %q: use an absolute path other than /", dir), map[string]any{"dir": dir})
}

// normalizeInstallServerURL validates and canonicalizes the control-plane base
// URL before it is persisted or embedded in a shell command. Query strings,
// fragments, and userinfo are not meaningful for the bootstrap endpoint and
// make accidental command/URL composition harder to reason about.
func normalizeInstallServerURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", productError(ErrorServerURLRequired, "server_url is required: the address the agent dials", nil)
	}
	invalid := func(reason string) error {
		return productError(ErrorServerURLInvalid, "server_url "+reason, map[string]any{"url": raw})
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", invalid("must be an absolute http(s) URL")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", invalid("must use http:// or https://")
	}
	if u.User != nil {
		return "", invalid("must not contain userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", invalid("must not contain a query or fragment")
	}
	return raw, nil
}

func (a *apiHandlers) listRejects(w http.ResponseWriter, r *http.Request) {
	if a.d.RecentRejects == nil {
		writeJSON(w, http.StatusOK, dto.NewList[dto.AgentReject](nil))
		return
	}
	writeJSON(w, http.StatusOK, dto.NewList(a.d.RecentRejects()))
}

func (a *apiHandlers) listUpgradeErrors(w http.ResponseWriter, r *http.Request) {
	if a.d.UpgradeErrors == nil {
		writeJSON(w, http.StatusOK, map[string]string{})
		return
	}
	writeJSON(w, http.StatusOK, a.d.UpgradeErrors())
}

func (a *apiHandlers) getNode(w http.ResponseWriter, r *http.Request) {
	uuid := chi.URLParam(r, "uuid")
	n, err := a.d.Store.NodeWithGPUs(uuid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	view := toNodeDTO(n)
	if claims, ok := a.claimsFromRequest(r); ok && claims.Role == "admin" {
		view = toNodeDTOWithToken(n)
	}
	views := []dto.Node{view}
	a.fillResolvedNodeFields([]store.Node{*n}, views)
	writeJSON(w, http.StatusOK, views[0])
}

// nodeDoctorTimeout is how long the doctor RPC is waited for. The ordinary
// 30-second RPC budget is too short for it: doctor runs CUDA and multi-GPU
// checks, and on a host whose driver answers slowly (a card that failed to
// initialize holds every client on the driver lock) its GPU queries alone can
// take tens of seconds. The agent bounds its own run below this (see
// doctorBudget in internal/agent/app), so a slow host returns a report rather
// than a timeout.
const nodeDoctorTimeout = 2 * time.Minute

func (a *apiHandlers) nodeDoctor(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	uuid := chi.URLParam(r, "uuid")
	ctx, cancel := rpccontext.WithTimeout(r.Context(), nodeDoctorTimeout)
	defer cancel()
	raw, err := a.d.CallNode(ctx, uuid, proto.RPCNodeDoctor, map[string]any{})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	// The agent's structured report is passed through as the agent typed it;
	// an agent that predates it sends only the text report.
	var out proto.DoctorReport
	if err := json.Unmarshal(raw, &out); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) updateNodeRuntime(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	uuid := chi.URLParam(r, "uuid")
	var req struct {
		Runtime string `json:"runtime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	preference := strings.ToLower(strings.TrimSpace(req.Runtime))
	if strings.TrimSpace(req.Runtime) == "" || !proto.ValidRuntimePreference(preference) {
		writeProductError(w, http.StatusBadRequest, ErrorNodeRuntimeInvalid, "runtime must be auto, docker, nerdctl, or udocker (runveil)", nil)
		return
	}
	raw, err := a.d.CallNode(r.Context(), uuid, proto.RPCNodeSetRuntime, map[string]string{"runtime": preference})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	var result map[string]string
	if err := json.Unmarshal(raw, &result); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	n, err := a.d.Store.NodeWithGPUs(uuid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err := a.d.Store.SetNodeRuntimePreference(uuid, preference); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The agent status heartbeat is authoritative and will update the stored
	// runtime shortly; return an immediate view for the selector.
	n.RuntimeDriver = result["runtime"]
	n.RuntimePreference = preference
	writeJSON(w, http.StatusOK, toNodeDTO(n))
}

func (a *apiHandlers) upgradeNode(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	uuid := chi.URLParam(r, "uuid")
	if a.d.Online != nil && !a.d.Online(uuid) {
		writeProductError(w, http.StatusConflict, ErrorNodeOffline, "node is offline", nil)
		return
	}
	if a.d.NodeStreams != nil && a.d.Store != nil {
		node, err := a.d.Store.NodeByUUID(uuid)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		pushed, err := a.d.NodeStreams.UpgradeAgent(uuid, node.OS, node.Arch)
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		if pushed {
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "upgrading"})
			return
		}
	}
	raw, err := a.d.CallNode(r.Context(), uuid, proto.RPCAgentUpgrade, map[string]any{})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	var result any
	if len(raw) > 0 && json.Unmarshal(raw, &result) == nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "upgrading", "result": result})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "upgrading"})
}

func (a *apiHandlers) renameNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSONBody(w, r, &req) || !validateNodeName(w, req.Name) {
		return
	}
	n, err := a.d.Store.RenameNode(chi.URLParam(r, "uuid"), req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Renaming is available to every management user.  A node token is a
	// credential, so do not include it in this ordinary mutation response;
	// token material is only returned by the admin-only regenerate endpoint (or
	// an admin node detail request).
	writeJSON(w, http.StatusOK, toNodeDTO(n))
}

func (a *apiHandlers) regenerateNodeToken(w http.ResponseWriter, r *http.Request) {
	n, token, err := a.d.Store.RegenerateNodeToken(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.d.KickNode != nil {
		a.d.KickNode(chi.URLParam(r, "uuid"))
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": toNodeDTOWithToken(n), "token": token})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError answers err as the API does: a product error with its stable
// code and params, any other error with its text. It is for a handler in
// front of the router (the experience environment's guard), so a refusal
// there reads like one from the API.
func WriteError(w http.ResponseWriter, status int, err error) { writeErr(w, status, err) }

func writeErr(w http.ResponseWriter, code int, err error) {
	response := map[string]any{"error": err.Error()}
	var productErr *messages.ProductError
	if errors.As(err, &productErr) {
		response["code"] = productErr.Code()
		w.Header().Set(usageErrorHeader, productErr.Code())
		if params := productErr.Params(); len(params) > 0 {
			response["params"] = params
		}
	}
	writeJSON(w, code, response)
}

const bytesPerGiB = 1024 * 1024 * 1024

func toNodeDTO(n *store.Node) dto.Node {
	return nodeDTO(n, false)
}

func toNodeDTOWithToken(n *store.Node) dto.Node {
	return nodeDTO(n, true)
}

func nodeDTO(n *store.Node, includeToken bool) dto.Node {
	d := dto.Node{
		UUID:                  n.UUID,
		Scheduling:            nodeSchedulingName(n.Scheduling),
		Name:                  n.Name,
		State:                 n.ConnectionState(),
		Hostname:              n.Hostname,
		OS:                    n.OS,
		Kernel:                n.Kernel,
		Arch:                  n.Arch,
		CPUModel:              n.CPUModel,
		CPUCores:              n.CPUCores,
		MemTotalGiB:           float64(n.MemTotalKB) * 1024 / bytesPerGiB,
		DiskTotalGiB:          float64(n.DiskTotalKB) * 1024 / bytesPerGiB,
		GPUCount:              n.GPUCount,
		GPUVendor:             nodeDTOGPUVendor(n),
		GPUDriver:             nodeDTOGPUDriver(n),
		NvidiaDriver:          n.NvidiaDriver,
		Framework:             n.Framework,
		FrameworkVersion:      n.FrameworkVersion,
		RuntimeDriver:         n.RuntimeDriver,
		RuntimePreference:     n.RuntimePreference,
		RuntimePlugin:         n.RuntimePlugin,
		Runtimes:              nodeDTORuntimes(n),
		PartitionCapabilities: partitioncap.ForNode(n),
		ManagementAddress:     managementIP(*n),
		ServerURL:             n.ServerURL,
		ConnectMode:           n.ConnectMode,
		ServerDialAddress:     n.ServerDialAddress,
		ServerDialListen:      n.ServerDialListen,
		AgentVersion:          n.AgentVersion,
		LastRemoteAddr:        n.LastRemoteAddr,
		LastRejectReason:      n.LastRejectReason,
		LastRejectAt:          utcTimePtr(n.LastRejectAt),
		LastSeenAt:            utcTimePtr(n.LastSeenAt),
		RecoveryPhase:         n.RecoveryPhase,
		RecoveryAttempts:      n.RecoveryAttempts,
		LastProbeAt:           utcTimePtr(n.LastProbeAt),
		LastRecoveryError:     n.LastRecoveryError,
		HasSSHCredential:      n.HasSSHCredential,
		CreatedAt:             utcTime(n.CreatedAt),
		GPUs:                  make([]dto.GPU, 0, len(n.GPUs)),
	}
	d.Labels, _ = store.NodeLabels(n.Labels)
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	if includeToken {
		// The command carries the token, so it goes out only where the
		// token itself does.
		d.Token = n.Token
		d.InstallCommand = nodeInstallCommandFor(n)
	}
	if n.PathUsage != "" {
		var usage any
		if json.Unmarshal([]byte(n.PathUsage), &usage) == nil {
			d.PathUsage = usage
		}
	}
	d.RuntimeGPUBlocked = n.RuntimeGPUBlocked
	d.RuntimeGPUPlugin = accelerator.RuntimeGPUPlugin(n.GPUVendor, n.RuntimeDriver)
	if n.RuntimeFallbackFrom != "" {
		d.RuntimeFallbackFrom = n.RuntimeFallbackFrom
		d.RuntimeFallbackReason = n.RuntimeFallbackReason
		d.RuntimeFallbackPlugin = accelerator.RuntimeGPUPlugin(n.GPUVendor, n.RuntimeFallbackFrom)
	}
	d.AcceleratorPreference = n.AcceleratorPreference
	d.AcceleratorVendors = n.AcceleratorVendorList()
	d.GPUProbeError = n.GPUProbeError
	d.GPUProbeFailedAt = utcTimePtr(n.GPUProbeFailedAt)
	if n.TopoJSON != "" {
		d.Topology = dto.ParseNodeTopology(n.TopoJSON)
	}
	partitionReports := partitioncap.Decode(n)
	for i := range n.GPUs {
		g := &n.GPUs[i]
		arch := n.ArchOf(*g)
		evidence, guide := gpuHealthDetail(g)
		d.GPUs = append(d.GPUs, dto.GPU{
			Index:                 g.Index,
			UUID:                  g.UUID,
			Name:                  g.Name,
			ComputeCapability:     g.ComputeCapability,
			ArchFamily:            arch.Family,
			Vendor:                g.Vendor,
			ArchID:                arch.ID,
			MemoryTotalBytes:      g.MemoryTotal,
			PowerLimitWatts:       g.PowerLimitWatts,
			PCIeBDF:               g.PCIeBDF,
			NUMANode:              g.NUMANode,
			PartitionCapabilities: partitionReports.ForGPU(g),
			MIGEnabled:            g.MIGEnabled,
			Ownership:             g.Ownership,
			ForeignProcesses:      g.ForeignUse(),
			Schedulable:           g.CanTakeWork(),
			HealthStatus:          g.HealthStatus,
			HealthCode:            g.HealthCode,
			HealthMessage:         g.HealthMessage,
			HealthEvidence:        evidence,
			HealthGuide:           guide,
		})
	}
	return d
}

func nodeDTOGPUVendor(n *store.Node) string {
	if n.GPUVendor != "" {
		return n.GPUVendor
	}
	if n.NvidiaDriver != "" {
		return "nvidia"
	}
	for _, gpu := range append(n.OwnedGPUs(), n.GPUs...) {
		if gpu.Vendor != "" {
			return gpu.Vendor
		}
	}
	return ""
}

func nodeDTOGPUDriver(n *store.Node) string {
	vendor := nodeDTOGPUVendor(n)
	if n.GPUDriver != "" {
		return accelerator.NormalizeDriverVersion(vendor, n.GPUDriver)
	}
	return accelerator.NormalizeDriverVersion(vendor, n.NvidiaDriver)
}

func nodeDTORuntimes(n *store.Node) []string {
	if n.Runtimes != "" {
		var out []string
		if json.Unmarshal([]byte(n.Runtimes), &out) == nil && len(out) > 0 {
			return out
		}
	}
	if n.RuntimeDriver != "" {
		return []string{n.RuntimeDriver}
	}
	return nil
}
