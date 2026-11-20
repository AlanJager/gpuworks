// Package app is the server assembly root: it wires config, store, bus, hub and
// the HTTP surface, and owns startup/graceful-shutdown.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/obsingest"
	"gpuworks.ai/gpuworks/internal/server/offlinepkg"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/version"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/selflog"
	"gpuworks.ai/gpuworks/internal/server/agentbin"
	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/controller"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/healthcheck"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/provider"
	_ "gpuworks.ai/gpuworks/internal/server/provider/all" // register drivers
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/recovery"
	"gpuworks.ai/gpuworks/internal/server/slurmsweep"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/webterm"
	"gpuworks.ai/gpuworks/internal/server/webui"
)

// App holds the assembled server.
type App struct {
	cfg           Config
	routes        http.Handler // the API router (RouteInventory)
	log           *slog.Logger
	store         *store.Store
	bus           *bus.Bus
	hub           *hub.Hub
	ctrl          *controller.Controller
	agentUpgrades *agentupgrade.Service
	obs           *observability
	ops           *opsStats // usage analytics' long-term statistics
	recovery      *recovery.Recoverer
	// proxyAcct is proxy request accounting (see accounting.go in the proxy
	// package).
	proxyAcct *proxy.Accountant
	proxy     *proxy.Proxy
	health    *healthcheck.Checker // the deployments' health checks
	cards     *cardHealth          // each card's checks and their failure records
	batches   *batch.Service       // the Batch API's executor and files
	telemetry *telemetryService    // the request trace export
	http      *http.Server
	// ln is created by Run instead of http.Server.ListenAndServe so tests can
	// bind an ephemeral port and discover the actual address through Addr.
	// trackingListener also lets shutdown close hijacked agent websocket
	// connections, which net/http otherwise leaves alive.
	lnMu sync.RWMutex
	lns  []*trackingListener
	// settings serves server.yaml to the API; restart cancels Run so the
	// service manager starts the process again.
	settings *serverSettings
	// lifetime separates the assembled resources from one cancellable Run.
	lifetime appLifetime
	// selfLog captures the server's own error-level slog records into the
	// logstore under source=server; nil disables the drain loop.
	selfLog *selflog.Collector
	// usage is the user experience improvement program's daily report;
	// notices the console's notice bell.
	usage   *usageReporter
	notices *noticeService
	// scaleReplicas sets a deployment's replica count as the API does
	// (planning.Scaling, which the handlers share), for the scheduling
	// automation's replica schedules.
	scaleReplicas func(deploymentID uint, role string, replicas int) error
	// ext is what the extension modules registered (extension_start.go).
	ext *extensionSet
	// slurmSweep removes the Slurm nodes that left their cluster, asking the
	// cloud about the nodes of entries made from a cloud cluster.
	slurmSweep *slurmsweep.Sweeper
}

// New builds the server from config.
func New(cfg Config, log *slog.Logger) (a *App, err error) {
	cfg = normalizeRuntimeConfig(cfg)
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	// Derive the DB path from the (possibly overridden) data dir when unset.
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "gpuworks.db")
	}
	b := bus.New()
	st, master, err := openCredentialStore(cfg, b, log)
	if err != nil {
		return nil, err
	}
	a = &App{cfg: cfg, log: log, store: st, bus: b, selfLog: cfg.SelfLog}
	defer func(built *App) {
		if err != nil {
			_ = built.Close()
		}
	}(a)
	// The node lists, the dashboard and the monitor share one snapshot of the
	// fleet instead of each reading every node and card per request.
	st.EnableFleetNodeSnapshot(nil)
	embedded, err := startEmbeddedCatalogs(st, log)
	if err != nil {
		return nil, err
	}
	// Bring stored deployments to the current launch-configuration shape before
	// anything reads one. The controller launches instances straight from these
	// columns, so a row still in the old shape would be started from a
	// configuration that is missing values nobody merges in any more.
	if err := normalizeLaunchConfigs(st); err != nil {
		return nil, err
	}
	catalogPackages, err := packages.New(filepath.Join(cfg.DataDir, "catalog-packages"))
	if err != nil {
		return nil, err
	}
	// Materialize the exact package embedded in this binary before updating the
	// database projection. A later server version then keeps this directory as
	// long as an older deployment or instance snapshot still references it.
	embeddedCatalog, embeddedDigest, err := seed.Package()
	if err != nil {
		return nil, err
	}
	if _, err := catalogPackages.PutLoaded(embeddedCatalog, embeddedDigest); err != nil {
		return nil, err
	}
	if err := embedded.finish(st); err != nil {
		return nil, err
	}
	if recovered, err := st.RecoverServerBenchmarkRuns(); err != nil {
		return nil, err
	} else if recovered > 0 {
		log.Warn("recovered orphaned server benchmarks", "count", recovered)
	}
	// Express the configured model endpoint as site rules, the one place a
	// node's download addresses come from, so node downloads stay hermetic
	// without a second, implicit endpoint setting.
	if strings.TrimSpace(cfg.ModelEndpoint) != "" {
		if err := ensureModelEndpointRules(st, strings.TrimSpace(cfg.ModelEndpoint)); err != nil {
			return nil, err
		}
	}
	// One network service for every component, so they all resolve against
	// the same catalog: every network declared in the store.
	networks := network.New(st, network.StoreCatalog{Store: st})
	readPrivateHosts := privateHostPermissions(cfg, log)
	h, err := newPolicyHub(st, log, cfg.PeerRelay, networks, readPrivateHosts)
	if err != nil {
		return nil, err
	}
	// Besides the listed proxies, the public entry point's connector on this
	// host while `gpuworks expose` has it enabled (ADR 0049).
	proxies, invalidProxies := auth.NewProxyTrust(cfg.TrustedProxies, expose.NewStore(cfg.DataDir).WatchEnabled().Enabled)
	for _, raw := range invalidProxies {
		log.Warn("invalid trusted proxy, ignored", "value", raw)
	}
	h.SetProxyTrust(proxies)
	modelLogoDir := filepath.Join(cfg.DataDir, "model-logos")
	if err := os.MkdirAll(modelLogoDir, 0o755); err != nil {
		return nil, err
	}
	if referenced, referenceErr := st.ReferencedCatalogPackageDigests(); referenceErr != nil {
		log.Warn("list referenced inference catalog packages during startup", "error", referenceErr)
	} else if removed, pruneErr := catalogPackages.Prune(referenced); pruneErr != nil {
		log.Warn("prune inference catalog packages during startup", "error", pruneErr)
	} else if len(removed) > 0 {
		log.Info("pruned unreferenced inference catalog packages during startup", "count", len(removed))
	}
	h.SetRuntimeAssetLoader(func(packageDigest, modelID, file string) ([]byte, error) {
		return catalogPackages.ReadAsset(strings.ToLower(strings.TrimSpace(packageDigest)), modelID, file)
	})
	deploy.SetDefaultImage(deploy.EngineVLLM, cfg.VLLMImage)
	ctrl := controller.New(st, h, log)
	ctrl.SetNetworks(networks)
	h.SetEnginePorts(ctrl.EnginePorts)
	px := proxy.New(st, networks, h, log, cfg.ProxyMaxBodyBytes)
	a.proxy = px
	ctrl.SetDrainProbe(px)

	// Credential encryption for cloud providers (master secret persisted in data dir).
	box, err := auth.NewSecretbox(master)
	if err != nil {
		return nil, err
	}
	st.SetSecretbox(box)
	// The network settings' proxies become proxy entries once the key that
	// seals their passwords is there; until that succeeds they stay put.
	if err := st.CarryOverNetworkProxies(); err != nil {
		log.Warn("carry the network settings' proxies over to the pull settings", "err", err)
	}
	ensurePlatformSSHKey(st, log)
	warnUnmanagedStoragePaths(st, cfg.StorageProbeRoots, log)
	jwtSigner, err := auth.NewTokenSigner(master, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	provSvc := provider.NewService(st, box)
	if err := provider.UseDataDir(cfg.DataDir); err != nil {
		log.Warn("cloud provider state stays in memory", "err", err)
	}

	ctrl.SetInferenceNodeLifecycleHandler(provSvc)
	// Metrics + logs stores, wired to the hub ingest sinks.
	obs, err := openObservability(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	a.obs = obs
	diag := newDiagnostics(cfg, st, h, obs, px, log)
	obs.SetDeploymentResolver(obsingest.WrapDeploymentLookup(func(instanceUUID string) (uint, error) {
		inst, err := st.InstanceByUUID(instanceUUID)
		if err != nil {
			return 0, err
		}
		return inst.DeploymentID, nil
	}, store.ErrNotFound))
	h.SetSinks(func(instanceUUID string, samples []proto.MetricSample) {
		obs.IngestMetrics(instanceUUID, samples)
		px.ObserveEngineMetrics(samples)
	}, obs.IngestLogs)
	h.SetLogIngestWait(obs.IngestLogsWait)
	h.SetBenchmarkDetailSink(newAgentBenchmarkDetailSink(filepath.Join(cfg.DataDir, "benchmark-artifacts"), st).Handle)

	// Proxy request accounting (plan-9-18-monitor-center.md §4.3): counts
	// completed /v1 requests per deployment+class and flushes to tsdb every
	// 10s through the same ingest path agent-reported metrics use.
	acct := proxy.NewAccountant(proxyMetricsSink(obs), log)
	px.SetAccountant(acct)
	if err := a.openRequestSinks(readPrivateHosts); err != nil {
		return nil, err
	}
	ops, telemetry := a.ops, a.telemetry

	ai := newAIService(st, obs, box, h.Online)
	ai.log = log
	ai.readPrivateHosts = readPrivateHosts
	health := healthcheck.New(healthSource{store: st}, healthSender{proxy: px})
	// The deployment and instance lists, the dashboard, the monitor and the
	// batches' model list share one snapshot of every deployment and instance.
	lists := deployment.NewListsWithConnections(st, modelPreparation{h, ctrl}, nil, h.Online)
	batches := batch.New(batchRepo{st}, batchSender{px}, batch.Config{Dir: filepath.Join(cfg.DataDir, "batch-files"), Models: batchModels{st, modelPreparation{h, ctrl}, lists}, PriorityClasses: priorityClassExists(st), Log: log})
	monSvc := newMonitorAPIServiceOn(lists, st, obs.TSDB(), nil, newRequestSource(st, px.LiveRequests, obs.UX()), health.Health, modelPreparation{h, ctrl}, h.Online)

	a.hub, a.ctrl, a.proxyAcct, a.health, a.batches = h, ctrl, acct, health, batches
	a.slurmSweep = newSlurmSweep(log, st, provSvc, h.Kick)
	var restart func()
	if cfg.SelfRestart {
		restart = a.requestRestart
	}
	a.settings = newServerSettings(cfg, restart, st.SnapshotTo)
	a.settings.readPrivateHosts = readPrivateHosts
	a.settings.installID = installIDOf(master)
	outside := outsideSources(st, readPrivateHosts, cfg.Distribution)
	a.usage, a.notices = newImprovementProgram(st, obs.UX(), a.settings, outside.Channel, log, h.SessionCount)
	h.SetUsageCounters(a.usage.Counters())
	if a.cards, err = newCardHealth(log, st, h); err != nil {
		return nil, err
	}

	dlDir := filepath.Join(cfg.DataDir, "dl")
	_ = os.MkdirAll(dlDir, 0o755)
	readAgent := func(name string) ([]byte, error) { return agentbin.Read(dlDir, name) }
	provSvc.SetAgentBinaryReader(readAgent)
	h.SetAgentBinaryReader(readAgent)
	a.agentUpgrades, err = newAgentUpgrades(st, h, cfg.DataDir, log)
	if err != nil {
		return nil, err
	}

	sshHostKeys, err := provision.NewFileHostKeyStore(filepath.Join(cfg.DataDir, "ssh-hostkeys.json"))
	if err != nil {
		return nil, err
	}
	nodePrechecker := provision.NewSSHPrechecker()
	nodePrechecker.HostKeys = sshHostKeys
	nodeInstaller := provision.NewSSHInstaller(readAgent)
	nodeInstaller.HostKeys = sshHostKeys
	nodeInstaller.UdockerBundleSource = cfg.UdockerBundleSource
	provision.SetSSHProxy(cfg.SSHProxy)
	provSvc.ConfigureDrivers(nodeInstaller)
	recoverer := recovery.New(st, h, provSvc, nodeInstaller, log, recovery.Config{
		Enabled: cfg.RecoveryEnabled, GracePeriod: cfg.RecoveryGracePeriod,
		InitialInterval: cfg.RecoveryInitialInterval, BackoffMultiplier: cfg.RecoveryBackoffMultiplier,
		MaxInterval: cfg.RecoveryMaxInterval, RestartWait: cfg.RecoveryRestartWait,
	})
	a.recovery = recoverer

	metadataClient := &http.Client{Transport: outbound.TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return planning.ControlPlaneNetwork(st) }, readPrivateHosts)}
	apiDeps := api.Deps{
		NodeDoctor: newNodeDoctor(st, h, nodeInstaller),

		Store:          st,
		Bus:            b,
		Log:            log,
		SessionCount:   h.SessionCount,
		AgentConnect:   h.ServeWS,
		InstallSh:      a.installHandler(),
		RecentRejects:  func() []dto.AgentReject { return agentRejectDTOs(h.RecentRejects()) },
		AgentUpgrades:  api.AgentUpgradeHooks{Service: agentUpgradeAPI{service: a.agentUpgrades}, Errors: h.UpgradeErrors},
		UIHandler:      withDL(dlDir, offlinepkg.NewServer(filepath.Join(cfg.DataDir, "offline"), version.String(), readAgent), webui.HandlerFS(cfg.Extensions.ui()), a.agentUpgrades.DownloadBinary),
		Online:         h.Online,
		Orders:         ctrl.Orders(),
		KickNode:       h.Kick,
		NodeSync:       h,
		CallNode:       h.CallNode,
		ModelTransfers: ctrl.Transfers(),
		PeerSync: api.PeerSyncHooks{
			SyncImageWithRate: h.SyncImageFromRate,
			ProbeImage:        h.ProbeImagePath,
			ProbeImageResult:  h.ProbeImagePathResult,
			RetryImage:        h.RetryImageSync,
		},
		Preparation: modelPreparation{h, ctrl},
		Lists:       lists,
		NodeRuntime: h,
		Outside:     outside,
		ImageFetch: api.ImageFetchHooks{
			Pull:   h.PullImage,
			Cancel: h.CancelImageTransfer,
			Remove: h.RemoveImageFromNode,
		},
		InspectImage:             h.InspectImage,
		ImageExists:              h.ImageExists,
		CatalogPackages:          catalogPackages,
		ModelLogoDir:             modelLogoDir,
		ForgetInitialCredentials: forgetInitialCredentials(cfg),
		FetchModelMeta: func(ctx context.Context, source, repo, revision, subfolder string, candidates []string) (*modelmeta.Meta, error) {
			return modelmeta.FetchFrom(ctx, source, repo, revision, subfolder, candidates, metadataClient, planning.ModelLogins(st))
		},
		ServerDialAgent:      h.DialAgentWithSecret,
		NodeStreams:          peerTransferStreams{h},
		OpenTerminal:         h.OpenTerminal,
		NodeCaps:             h.NodeCaps,
		Terminal:             webterm.New(st),
		WakeHeld:             wakeHeldBy(px),
		ProxyV1:              dataPlane(px.ModelsHandler(), batches.V1Handler(batchAuth(px)), px.Handler()),
		Batches:              batchConsole{batches},
		PlaygroundV1:         px.PlaygroundHandler(box),
		NodePrecheck:         nodePrechecker,
		NodeInstaller:        connectionInstaller(nodeInstaller, cfg.DataDir),
		Providers:            provSvc,
		Networks:             networks,
		Server:               a.settings,
		MetricsQuery:         obs.queryMetrics,
		MetricsLatest:        obs.latestMetrics,
		MetricsIngest:        obs.IngestMetrics,
		LogsSearch:           obs.searchLogs,
		LogsContext:          obs.contextLogs,
		LogsHistogram:        obs.histogramLogs,
		Monitor:              monitorService{monSvc},
		Telemetry:            telemetry,
		Analytics:            newAnalyticsAPI(ops, st, obs, cfg.DataDir),
		SchedulingActivity:   ops.sched,
		CardHealth:           a.cards,
		Experience:           experienceLauncher(cfg, log),
		UsageReport:          a.usage,
		Notices:              a.notices,
		AI:                   ai,
		CatalogTemplates:     catalogTemplates{store: st, probe: templateProbe(px)},
		AuthSigner:           jwtSigner,
		AISecretbox:          box,
		BenchmarkArtifactDir: filepath.Join(cfg.DataDir, "benchmark-artifacts"),
		Recovery:             recoverer,
		Diagnostics:          diag,
	}
	if err := startExtensions(cfg, a, master, &apiDeps); err != nil {
		return nil, err
	}
	handler := a.apiRouter(apiDeps, auth.SessionPolicy{IdleTimeout: cfg.SessionIdleTimeout, MaxLifetime: cfg.SessionMaxLifetime, Proxies: proxies, Cookie: cfg.sessionCookie()})
	a.scaleReplicas = planning.Scaling{Store: st, Resolve: networks.Resolve, Online: h.Online, UCXNetlinkHang: h.UCXNetlinkHang}.ScaleScheduled
	handler = api.WithSecurityHeaders(proxies, accessLogHandlerFor(log, cfg.DataDir, a.routeByFunction(cfg.experienceGuard(handler))))

	// No read/write timeouts: the agent WS and SSE are long-lived. The WS
	// route bypasses these via hijacking; SSE flushes keepalives.
	a.http = &http.Server{Handler: handler}
	return a, nil
}

// Run starts the HTTP server and background loops, blocking until ctx is done.
func (a *App) Run(ctx context.Context) error {
	ctx, finish, err := a.lifetime.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	tlsConfig, err := a.cfg.listenerTLS()
	if err != nil {
		return err
	}
	// Bind before starting any background loops. This makes a failed bind a
	// synchronous startup error and, for Addr :0, publishes the real port before
	// the first agent is started by an in-process harness.
	lns, err := listenAll(a.cfg.bindings())
	if err != nil {
		return err
	}
	a.lnMu.Lock()
	if a.lns != nil {
		a.lnMu.Unlock()
		closeAll(lns)
		return errors.New("server app already running")
	}
	// net/http marks a Server as permanently shut down after Shutdown. Build a
	// fresh wrapper on every Run so a test harness (and an embeddable caller)
	// can restart the same assembled app on its existing store and handler.
	handler := a.http.Handler
	tracked, waitRequests := trackRunRequests(ctx, handler)
	httpServer := &http.Server{Handler: tracked, ConnContext: connFunctions, TLSConfig: tlsConfig}
	a.http = httpServer
	defer func() { a.http.Handler = handler }()
	a.lns = lns
	a.lnMu.Unlock()
	a.hub.StartSessions()
	a.proxy.Start()
	defer a.proxy.Close()
	defer func() {
		closeAll(lns)
		a.lnMu.Lock()
		a.lns = nil
		a.lnMu.Unlock()
	}()

	// bg tracks every loop started below that runs on the shared ctx; see
	// backgroundGroup in shutdown.go (proxyAcct/RunMetrics track their own).
	var bg backgroundGroup
	goBG := bg.start

	// Background: flip stale-online nodes to offline as a safety net.
	goBG(func() { a.staleLoop(ctx) })
	goBG(func() { a.eventRetentionLoop(ctx) }) // Background: bound the deployment event timeline.
	goBG(func() { a.clusterNodeLoop(ctx) })    // Background: remove Kubernetes and Slurm nodes that left their cluster.
	// Background: reconnect nodes that listen for reverse server_dial sessions.
	goBG(func() { a.hub.RunServerDialReconnect(ctx, a.cfg.ServerDialReconnectInterval) })
	goBG(func() { a.hub.RunNodeQueues(ctx) }) // persist reports queued off the read loops
	// Background: the independent SSH path after ordinary reconnects exhaust.
	goBG(func() { a.recovery.Run(ctx) })
	// The public entry point's connector service (`gpuworks expose`) runs on
	// its own and forwards to what this server really bound.
	if err := expose.RecordServerOrigin(a.cfg.DataDir, exposeOrigin(lns)); err != nil {
		a.log.Warn("cannot record the listening address for `gpuworks expose`", "err", err)
	}
	// Background: reconcile deployments → instances.
	goBG(func() { a.ctrl.Run(ctx) })
	goBG(func() { a.agentUpgrades.Run(ctx) })
	// Background: verify each PD prefill instance's KV handoff once.
	goBG(func() { runPDVerifier(ctx, a.store, a.proxy, a.obs.TSDB()) })
	// Background: schedules, waiting wakes, residency.
	waits, orders := a.ctrl.PlacementWaits(), a.ctrl.Orders()
	goBG(func() {
		runAutomation(ctx, a.store, a.proxy, a.proxyAcct, a.obs.hostMemory(), a.scaleReplicas, waits, orders, a.log)
	}) // Background: schedules, waiting wakes, residency.
	for _, run := range a.services() {
		goBG(func() { run(ctx) }) // Background: health checks, card records, usage report, notices, extension services.
	}
	// Background: the batches in progress, resumed from their checkpoints.
	if err := a.batches.Start(ctx); err != nil {
		a.log.Error("batch executor not started", "err", err)
	} else {
		goBG(func() { <-ctx.Done(); a.batches.Wait() })
	}
	// Background: prune metrics/logs beyond the retention window
	// (--metrics-retention, default 72h).
	//
	// RunMetrics deliberately runs on its OWN context (metricsCtx),
	// never the shared shutdown ctx: the proxy accountant's final flush (see
	// acctDone below) is only enqueued into the metrics ingest queue partway
	// through the accountant's OWN ctx.Done handling, which takes up to
	// proxy.finalFlushWaitTimeout. If RunMetrics reacted to the same
	// ctx, it would very likely have already drained whatever was queued at
	// that instant and returned - with nothing left running to ever pick up
	// the accountant's later enqueue, silently dropping it. Keeping it on a
	// separate context lets Run stop it explicitly, after stopProxyAccounting
	// confirms the accountant (and therefore its final enqueue) is done - see
	// stopMetricsIngest.
	metricsCtx, metricsCancel := context.WithCancel(context.Background())
	logsCtx, logsCancel := context.WithCancel(context.Background())
	defer logsCancel()
	// Every stop path drains metrics explicitly; the defer also covers future
	// early returns while bringing up the remaining loops.
	defer metricsCancel()
	metricsDone := make(chan struct{})
	logsDone := make(chan struct{})
	if a.obs != nil {
		goBG(func() { a.ops.run(ctx) })
		goBG(func() { a.ops.backfill(ctx) })
		goBG(func() { a.ops.replayScheduling(ctx) })
		go func() {
			defer close(logsDone)
			a.obs.RunLogs(logsCtx)
		}()
		go func() {
			defer close(metricsDone)
			a.obs.RunMetrics(metricsCtx)
		}()
		// Background: the server's own error-level slog records become
		// searchable server-source logs instead of living only in the
		// process's rotated file.
		goBG(func() { a.obs.RunSelfLogs(ctx, a.selfLog) })
	} else {
		close(metricsDone)
		close(logsDone)
	}
	// Accounting stops after requests have actually returned, including ones
	// whose HTTP connection was force-closed. Its final enqueue then precedes
	// the metrics loop's final drain (stopRun).
	acctCtx, acctCancel := context.WithCancel(context.Background())
	defer acctCancel()
	acctDone := make(chan struct{})
	go func() {
		defer close(acctDone)
		a.proxyAcct.Run(acctCtx)
	}()

	errc := make(chan error, len(lns))
	var serving backgroundGroup
	defer serving.wait()
	for _, ln := range lns {
		serving.start(func() {
			a.log.Info("server listening", "addr", ln.Addr().String(), "functions", ln.functions)
			var err error
			if tlsConfig != nil && publicTLSListener(ln) {
				err = httpServer.ServeTLS(ln, "", "")
			} else {
				err = httpServer.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		})
	}

	select {
	case <-ctx.Done():
		a.log.Info("shutting down")
		err = nil
	case err = <-errc:
	}
	a.lifetime.stop()
	a.stopRun(lns, waitRequests, acctCancel, acctDone, metricsCancel, metricsDone, logsCancel, logsDone, &bg)
	return err
}
