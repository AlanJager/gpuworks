// Package conn is the agent's side of the server link: it dials the WS route,
// layers yamux, opens the ctrl stream, performs the per-node token handshake,
// and then runs heartbeat + status reporting while
// serving inbound control messages. The reconnect loop lives in the app package.
package conn

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/netinfo"
	"gpuworks.ai/gpuworks/internal/agent/sysinfo"
	"gpuworks.ai/gpuworks/internal/agent/terminal"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/version"
	"gpuworks.ai/gpuworks/internal/wsconn"
)

// Credentials the agent presents on connect.
type Credentials struct {
	ServerURL   string
	NodeUUID    string
	NodeToken   string
	ConnectMode string
	Smoke       bool
	// DataLane enables the dedicated high-throughput data lane on agent_dial
	// connections (default true). server_dial and smoke never open one; against
	// an old server that issues no SessionID the agent silently stays single-lane.
	DataLane   bool
	BinaryPath string
	// ResumeArgs are the arguments the manual upgrade command restarts `run`
	// with (after "run"). The RPC upgrade path leaves this empty and preserves
	// os.Args verbatim.
	ResumeArgs []string
}

// RejectError is returned when the server explicitly rejects a handshake.
// Callers can use Reason to choose a long retry backoff without treating the
// rejection as an unexplained EOF.
type RejectError struct {
	Reason string
	Detail string
}

func (e *RejectError) Error() string {
	if e.Detail == "" {
		return "server rejected connection: " + e.Reason
	}
	return "server rejected connection: " + e.Reason + ": " + e.Detail
}

// OnRegistered is called when the server confirms the node identity for this
// connection. The identity is held in memory only.
type OnRegistered func(uuid, token string) error

// SendFunc sends a control-plane envelope on the current connection.
type SendFunc func(t string, d any) error

// Handlers receives desired-state and RPC callbacks and is bound to the live
// connection's sender for status reporting. A single Handlers instance persists
// across reconnects (owned by the agent app); Bind/Unbind track the current link.
type Handlers interface {
	Bind(send SendFunc)
	Unbind()
	OnStateSync(ctx context.Context, ds proto.DesiredState)
	// OnStateUpdate returns true when the effective image ref set changed and
	// its node-presence snapshot should be refreshed.
	OnStateUpdate(ctx context.Context, su proto.StateUpdate) bool
	OnRPC(ctx context.Context, method string, params json.RawMessage) (any, error)
	RuntimeDriverName() string
	// StreamLogs writes a container's logs (identified by ref = instance uuid)
	// into w until ctx is cancelled or the container ends.
	StreamLogs(ctx context.Context, ref string, follow bool, w io.Writer) error
	StreamModelArchive(ctx context.Context, path string, w io.Writer) error
	ReceiveModelArchive(ctx context.Context, path string, r io.Reader) error
	StreamImage(ctx context.Context, imageRef, transferID string, rw io.ReadWriter, load bool) error
	// StreamLargeFile writes one bounded regular file (an absolute path on this
	// host) into w. It is the read side of file.large_read; see proto for what
	// the bound is and why it is separate from the other two readers.
	StreamLargeFile(ctx context.Context, path string, w io.Writer) error
	// ReceiveStorageUpload serves a storage.upload stream: it reads the body
	// to its end and writes one StorageFilesStatus line (ADR 0053).
	ReceiveStorageUpload(ctx context.Context, hdr proto.StreamHeader, rw io.ReadWriter) error
	// SendStorageDownload serves a storage.download stream: one
	// StorageDownloadHead line, then the bytes unless the head refuses. It
	// returns a *proto.StorageFilesError exactly when the head carried a
	// refusal; any other error happened after the head.
	SendStorageDownload(ctx context.Context, hdr proto.StreamHeader, w io.Writer) error
}

type containerTerminalHandler interface {
	ContainerTerminalArgv(context.Context, string, []string) ([]string, error)
}

type containerTerminalSessionHandler interface {
	ContainerTerminalSession(context.Context, string, []string) ([]string, func(context.Context) error, error)
}

type containerTerminalCapabilities interface {
	ContainerTerminalCapabilities() []string
}

type runtimeAssetCache interface {
	CachedRuntimeAssetDigests() []string
}

// Conn is a single live connection attempt/session.
type Conn struct {
	creds    Credentials
	log      *slog.Logger
	onReg    OnRegistered
	handlers Handlers
	prober   gpu.Prober

	ysess               *yamux.Session
	ctrl                io.ReadWriteCloser
	dec                 *json.Decoder
	writeMu             sync.Mutex
	imageStatusRevision uint64 // guarded by writeMu
	statusMu            sync.Mutex
	statusRunning       bool
	statusTrailing      bool
	// enginePorts is the server's engine port range from the last state sync.
	enginePorts atomic.Pointer[proto.PortRange]

	// sessionID is the ctrl session id returned in Registered (empty against an
	// old server or a no-lane connection). It authorises the data lane.
	sessionID string
	// dataMu guards the current data-lane yamux session (nil when detached).
	dataMu   sync.Mutex
	dataSess *yamux.Session

	// localMu guards the local end of the connection to the server and whether
	// it goes through an HTTP proxy; they decide the management address.
	localMu  sync.Mutex
	local    net.IP
	viaProxy bool
}

// New creates a Conn (not yet connected). handlers may be nil for a
// node-only connection that serves no control requests; prober may be nil to
// use the default nvidia-smi prober.
func New(creds Credentials, onReg OnRegistered, handlers Handlers, prober gpu.Prober, log *slog.Logger) *Conn {
	if prober == nil {
		prober = gpu.New()
	}
	return &Conn{creds: creds, log: log, onReg: onReg, handlers: handlers, prober: prober}
}

// Run dials, handshakes, and serves until the connection drops or ctx is done.
// It returns the terminating error (nil on clean ctx cancellation).
func (c *Conn) Run(ctx context.Context) error {
	// A connection that will open a data lane dials the ctrl lane with the ctrl
	// profile and ?lane=ctrl; the server then issues a SessionID. Otherwise the
	// agent keeps the single adaptive data-profile session with no lane
	// parameter, so an old server sees exactly the request it expects.
	wantData := c.wantsDataLane()
	lane := proto.LaneData
	wsURL := toWSURL(c.creds.ServerURL) + "/api/v1/agent/connect"
	if wantData {
		lane = proto.LaneCtrl
		wsURL += "?lane=ctrl"
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	client, dialedFrom := localAddrClient()
	ws, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"User-Agent": []string{"gpuworks-agent/" + version.Version}},
	})
	if err != nil {
		return err
	}
	ws.SetReadLimit(-1) // yamux frames can exceed the default 32KiB limit
	netConn := wsconn.Wrap(websocket.NetConn(context.Background(), ws, websocket.MessageBinary))
	defer netConn.Close()
	c.setLocal(dialedFrom(), proxiedServer(c.creds.ServerURL))
	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			_ = netConn.Close()
		case <-stopClose:
		}
	}()

	return c.runYamuxClient(ctx, netConn, lane, wantData)
}

// wantsDataLane reports whether this connection should open a dedicated data
// lane: the escape hatch is on, it is not a smoke probe, and it is an agent_dial
// connection (server_dial keeps a single lane in the first phase).
func (c *Conn) wantsDataLane() bool {
	return c.creds.DataLane && !c.creds.Smoke && c.creds.ConnectMode != "server_dial"
}

// RunAccepted authenticates a server-initiated raw TCP connection, then serves
// the same yamux ctrl protocol used by the normal agent_dial WebSocket path.
func (c *Conn) RunAccepted(ctx context.Context, netConn net.Conn) error {
	defer netConn.Close()
	c.setLocal(netConn.LocalAddr(), false)
	if err := c.serverDialAgentAuth(netConn); err != nil {
		return err
	}
	// server_dial keeps a single adaptive data-profile lane and opens no data
	// lane; the server's dataLaneRoute "none" state routes every kind over
	// it — this is server_dial's permanent shape, not a failure fallback.
	return c.runYamuxClient(ctx, netConn, proto.LaneData, false)
}

func (c *Conn) runYamuxClient(ctx context.Context, netConn net.Conn, lane string, wantData bool) error {
	// The ctrl connection uses the profile selected by lane; a data lane, when
	// opened, always uses the adaptive data profile on its own socket.
	ycfg := laneYamuxConfig(lane, io.Discard)
	var err error
	c.ysess, err = yamux.Client(netConn, ycfg, nil)
	if err != nil {
		return err
	}
	defer c.ysess.Close()

	// Open the ctrl stream and write its header.
	c.ctrl, err = c.ysess.OpenStream(ctx)
	if err != nil {
		return err
	}
	if err := c.writeStreamHeader(proto.StreamHeader{Kind: proto.StreamCtrl}); err != nil {
		return err
	}
	c.dec = json.NewDecoder(bufio.NewReader(c.ctrl))

	if err := c.handshake(); err != nil {
		return err
	}
	if c.creds.Smoke {
		return nil
	}
	c.log.Info("connected to server", "node", c.creds.NodeUUID)
	c.confirmUpgrade()

	// Bind the persistent handlers to this connection's sender so the engine and
	// downloader can report status; unbind when the connection ends.
	if c.handlers != nil {
		c.handlers.Bind(func(t string, d any) error { return c.send(t, 0, d) })
		defer c.handlers.Unbind()
	}

	// Accept server-initiated streams (tunnel for /v1 proxy, logtail).
	go c.acceptStreams(ctx)

	// Open the data lane only when the server issued a SessionID (a current
	// server that accepted lane=ctrl). It runs in its own goroutine and reconnects
	// independently, never blocking the ctrl loops; it is cancelled when this
	// connection ends, which closes the data lane (a child of the ctrl session).
	if wantData && c.sessionID != "" {
		dctx, dcancel := context.WithCancel(ctx)
		defer dcancel()
		defer c.closeDataLane()
		go c.runDataLane(dctx)
	}

	// Report initial status immediately, then run the periodic loops.
	c.reportStatus(ctx)
	return c.serve(ctx)
}

func (c *Conn) serverDialAgentAuth(netConn net.Conn) error {
	secret := ""
	if c.creds.NodeUUID != "" && c.creds.NodeToken != "" {
		secret = proto.ServerDialNodeSecret(c.creds.NodeToken)
	}
	if secret == "" {
		// First server_dial enrollment has no issued UUID yet; prove the
		// per-node token directly and switch to the derived secret afterwards.
		secret = strings.TrimSpace(c.creds.NodeToken)
	}
	if secret == "" {
		return errors.New("server_dial requires node token")
	}
	nonce, err := randomNonce()
	if err != nil {
		return err
	}
	challenge := proto.ServerDialChallenge{NodeUUID: c.creds.NodeUUID, Nonce: nonce}
	line, err := json.Marshal(challenge)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := netConn.Write(line); err != nil {
		return fmt.Errorf("write server_dial challenge: %w", err)
	}
	respLine, err := readLine(netConn)
	if err != nil {
		return fmt.Errorf("read server_dial proof: %w", err)
	}
	var proof proto.ServerDialProof
	if err := json.Unmarshal(respLine, &proof); err != nil {
		return fmt.Errorf("decode server_dial proof: %w", err)
	}
	if !proto.VerifyServerDialHMAC(secret, nonce, proof.HMAC) {
		return errors.New("invalid server_dial proof")
	}
	return nil
}

func randomNonce() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (c *Conn) handshake() error {
	hello := proto.Hello{
		ProtoMin: proto.ProtoMin,
		ProtoMax: proto.ProtoMax,
		Version:  version.Version,
		Caps:     helloCapabilitiesForRuntime(c.prober, runtimeName(c.handlers)),
	}
	if h, ok := c.handlers.(containerTerminalCapabilities); ok {
		hello.Caps = append(hello.Caps, validatedContainerCaps(runtimeName(c.handlers), h.ContainerTerminalCapabilities())...)
	}
	if h, ok := c.handlers.(runtimeAssetCache); ok {
		hello.RuntimeAssetDigests = h.CachedRuntimeAssetDigests()
	}
	if c.creds.NodeToken != "" {
		hello.NodeUUID = c.creds.NodeUUID
		hello.NodeToken = c.creds.NodeToken
	}
	hello.Hostname = sysinfo.Collect().Hostname
	if h, ok := c.handlers.(interface{ OwnModelsDir() string }); ok {
		hello.OwnModelsDir = h.OwnModelsDir()
	}
	hello.ConnectMode = c.creds.ConnectMode
	hello.Smoke = c.creds.Smoke
	if err := c.send(proto.THello, 0, hello); err != nil {
		return err
	}

	// First reply is the registered identity, followed by the server hello.
	var env proto.Envelope
	if err := c.dec.Decode(&env); err != nil {
		return err
	}
	if env.T == proto.TReject {
		var rej proto.Reject
		if err := json.Unmarshal(env.D, &rej); err != nil {
			return err
		}
		return &RejectError{Reason: rej.Reason, Detail: rej.Detail}
	}
	if env.T == proto.TRegistered {
		var reg proto.Registered
		if err := json.Unmarshal(env.D, &reg); err != nil {
			return err
		}
		c.creds.NodeUUID = reg.NodeUUID
		if strings.TrimSpace(reg.NodeToken) != "" {
			c.creds.NodeToken = reg.NodeToken
		}
		c.sessionID = reg.SessionID
		if c.onReg != nil {
			if err := c.onReg(reg.NodeUUID, strings.TrimSpace(reg.NodeToken)); err != nil {
				return err
			}
		}
		c.log.Info("registered with server", "node", reg.NodeUUID)
		// Then read the server hello.
		if err := c.dec.Decode(&env); err != nil {
			return err
		}
	}
	if env.T != proto.THello {
		return errors.New("handshake: expected hello, got " + env.T)
	}
	return nil
}

// helloCapabilities describes the actual accelerator backend selected by the
// prober. The server uses this as a capability hint; hardware status remains
// authoritative from the subsequent status.node report.
func helloCapabilities(prober gpu.Prober) []string {
	return helloCapabilitiesForRuntime(prober, "")
}

// helloCapabilitiesForRuntime adds platform capabilities. Runtime-specific
// container terminal bits are appended by handshake from the handler's
// PTYExecer implementation and are validated against the runtime name below.
func helloCapabilitiesForRuntime(prober gpu.Prober, runtimeDriver string) []string {
	_ = runtimeDriver // runtime-specific container bits come from the handler
	caps := []string{"sysinfo"}
	// Terminal support is platform-gated rather than GPU-gated, so it is
	// advertised independently of the prober. The server greys out the terminal
	// entry point for agents that do not claim it, instead of opening a session
	// that can only go blank.
	if terminal.Supported() {
		caps = append(caps, proto.CapWebterm)
		// Container capabilities are intentionally not inferred from a runtime
		// name; the handler must provide the concrete PTY implementation.
	}
	caps = append(caps, proto.CapNetworkUpdate, proto.CapBench, proto.CapBenchWarmup, proto.CapBenchArrival, proto.CapBenchInteractivity, proto.CapBenchEvidence, proto.CapImagePullCandidates,
		proto.CapStorageFiles)
	if upgradeSupported() {
		caps = append(caps, proto.CapUpgradePush)
	}
	if prober == nil {
		return caps
	}
	vendor := strings.ToLower(strings.TrimSpace(prober.Vendor()))
	backend := map[string]string{
		"nvidia": "gpu.nvidia-smi",
		"ascend": "gpu.npu-smi",
		"ppu":    "gpu.ppu-smi",
		"thead":  "gpu.ppu-smi",
		"amd":    "gpu.amd-smi",
		"rocm":   "gpu.amd-smi",
		"hygon":  "gpu.hy-smi",
		"dcu":    "gpu.hy-smi",
	}[vendor]
	if backend == "" && vendor != "" {
		backend = "gpu." + vendor
	}
	if backend != "" {
		caps = append([]string{backend}, caps...)
	}
	return caps
}

func runtimeName(h Handlers) string {
	if h == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(h.RuntimeDriverName()))
}

func validatedContainerCaps(runtimeDriver string, caps []string) []string {
	if len(caps) == 0 {
		return nil
	}
	runtimeDriver = strings.ToLower(strings.TrimSpace(runtimeDriver))
	out := make([]string, 0, len(caps))
	for _, cap := range caps {
		switch cap {
		case proto.CapWebtermContainer:
			if runtimeDriver != "udocker" {
				out = append(out, cap)
			}
		case proto.CapWebtermContainerUdocker:
			if runtimeDriver == "udocker" {
				out = append(out, cap)
			}
		}
	}
	return out
}

// serve runs heartbeat + status tickers and reads inbound control messages.
func (c *Conn) serve(ctx context.Context) error {
	callerCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go c.heartbeatLoop(ctx)
	go c.statusLoop(ctx)

	// Inbound reader. Errors here (server closed, network drop) end the session.
	readErr := make(chan error, 1)
	go func() {
		for {
			var env proto.Envelope
			if err := c.dec.Decode(&env); err != nil {
				readErr <- err
				return
			}
			c.handleInbound(env)
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-readErr:
		// A caller that cancelled tears the connection down, so the read
		// may fail first; that is still a cancellation, not a lost session.
		// The caller's Done can close before cancellation propagates to our
		// child. The transport may report that shutdown in this short window.
		if callerCtx.Err() != nil {
			return nil
		}
		return err
	}
}

func (c *Conn) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.send(proto.THeartbeat, 0, proto.Heartbeat{UnixMS: time.Now().UnixMilli()}); err != nil {
				c.log.Debug("heartbeat send failed", "err", err)
				return
			}
		}
	}
}

func (c *Conn) statusLoop(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.requestStatus(ctx)
		}
	}
}

// reportStatus collects sysinfo + GPUs and sends a status.node envelope.
func (c *Conn) reportStatus(ctx context.Context) {
	sys := sysinfo.Collect()
	gpus, probeErr := c.prober.Probe(ctx)
	// Whether the vendor CLI answered, and whether that leaves the node's
	// accelerators unknown, are two questions. The optional queries below
	// go on the first; what the server is told goes on the second.
	cliAnswered := probeErr == nil
	probeOK := cliAnswered || gpu.CLIHasNothingToAnswer(gpus)
	probeError := ""
	if probeErr != nil && !probeOK {
		c.log.Warn("gpu probe failed", "err", probeErr)
		probeError = probeErr.Error()
	}
	// Findings only this agent can produce — whether the runtime can
	// actually use each card — are folded into the cards before they go.
	if h, ok := c.handlers.(interface {
		CardHealthFindings(context.Context, []proto.GPUInfo, bool)
	}); ok {
		h.CardHealthFindings(ctx, gpus, cliAnswered)
	}
	sys.GPUVendor = c.prober.Vendor()
	sys.GPUDriver = c.prober.DriverVersion(ctx)
	if p, ok := c.prober.(interface {
		Framework(context.Context) (string, string)
	}); ok {
		sys.Framework, sys.FrameworkVersion = p.Framework(ctx)
	}
	if sys.GPUVendor == "nvidia" {
		sys.NvidiaDriver = sys.GPUDriver
	}
	if c.handlers != nil {
		sys.RuntimeDriver = c.handlers.RuntimeDriverName()
		if h, ok := c.handlers.(interface {
			RuntimeGPU(context.Context) (string, string)
		}); ok {
			sys.RuntimePlugin, sys.RuntimeGPUBlocked = h.RuntimeGPU(ctx)
		}
		if h, ok := c.handlers.(interface{ RuntimeFallback() (string, string) }); ok {
			sys.RuntimeFallbackFrom, sys.RuntimeFallbackReason = h.RuntimeFallback()
		}
	}
	if sys.RuntimeDriver != "" {
		sys.Runtimes = []string{sys.RuntimeDriver}
	}
	if h, ok := c.handlers.(interface {
		AcceleratorVendors() []proto.AcceleratorPresence
	}); ok {
		sys.AcceleratorVendors = h.AcceleratorVendors()
	}
	if h, ok := c.handlers.(interface {
		PartitionAssets(context.Context) []proto.AssetReport
	}); ok {
		sys.PartitionAssets = h.PartitionAssets(ctx)
	}
	if h, ok := c.handlers.(interface {
		CachedPartitionProbes() []proto.PartitionProbeResult
	}); ok && probeErr != nil {
		// The server keeps the last GPU inventory when the probe fails; the
		// probe results that go with it are kept the same way.
		sys.PartitionProbes = h.CachedPartitionProbes()
	} else if h, ok := c.handlers.(interface {
		PartitionProbes(context.Context, []proto.AssetReport, []proto.GPUInfo) []proto.PartitionProbeResult
	}); ok {
		sys.PartitionProbes = h.PartitionProbes(ctx, sys.PartitionAssets, gpus)
	}
	sys.PartitionSafetyProtocol = 1
	imageRevision := c.imageRevision()
	var images []proto.ImagePresence
	var pathUsage []proto.PathUsage
	if h, ok := c.handlers.(interface {
		StatusSnapshot(context.Context, []string) ([]proto.ImagePresence, []proto.PathUsage)
	}); ok {
		// ImageRefs are supplied by state sync and retained by the runtime. The
		// optional callback uses the runtime's current catalog snapshot.
		images, pathUsage = h.StatusSnapshot(ctx, nil)
	}
	// Hermetic agents simulate node addresses, so they may supply their own
	// network inventory instead of the test host's.
	var network proto.NetInventory
	if h, ok := c.handlers.(interface{ NetworkInventory() proto.NetInventory }); ok {
		network = h.NetworkInventory()
	} else {
		network = netinfo.Collect()
		// A simulated inventory brings its own management address; the
		// test host's route must not leak into it.
		network.Management = c.management(network)
		// The runtime knows about the ib_write_bw embedded in the agent,
		// which a PATH lookup cannot see. Only a node with RDMA ports needs
		// it, so others never extract or probe it.
		if h, ok := c.handlers.(interface{ NetworkTools() []string }); ok && len(network.RDMAPorts) > 0 {
			network.Tools = h.NetworkTools()
		}
		network.EnginePortListeners = c.enginePortListeners()
		if p, ok := c.prober.(gpu.AcceleratorPortProber); ok && cliAnswered {
			network.AcceleratorPorts = p.AcceleratorPorts(ctx)
		}
	}
	st := proto.NodeStatus{Sys: sys, GPUs: gpus, GPUProbeOK: &probeOK, GPUProbeError: probeError, Images: images, PathUsage: pathUsage, Network: &network}
	if cliAnswered {
		st.TopoRaw = c.prober.Topology(ctx)
	}
	// Only a runtime whose daemon does the pulling has a proxy to report.
	if h, ok := c.handlers.(interface {
		RuntimeProxyStatus(context.Context) *proto.RuntimeProxyStatus
	}); ok {
		st.RuntimeProxy = h.RuntimeProxyStatus(ctx)
	}
	if err := c.sendNodeStatus(st, imageRevision); err != nil {
		c.log.Debug("status.node send failed", "err", err)
	}
}

func (c *Conn) handleInbound(env proto.Envelope) {
	if c.handlers == nil {
		return
	}
	ctx := context.Background()
	switch env.T {
	case proto.TStateSync:
		var ds proto.DesiredState
		if err := json.Unmarshal(env.D, &ds); err != nil {
			c.log.Warn("bad state.sync", "err", err)
			return
		}
		c.handlers.OnStateSync(ctx, ds)
		c.setEnginePorts(ctx, ds.Network.EnginePorts)
	case proto.TStateUpdate:
		var su proto.StateUpdate
		if err := json.Unmarshal(env.D, &su); err != nil {
			c.log.Warn("bad state.update", "err", err)
			return
		}
		imageRefsChanged := c.handlers.OnStateUpdate(ctx, su)
		if su.Network != nil {
			c.setEnginePorts(ctx, su.Network.EnginePorts)
		}
		if imageRefsChanged {
			// Catalog changes are comparatively rare and users expect node presence
			// to converge immediately. The periodic status loop is a 60-second safety
			// net, not the primary propagation path for image add/delete.
			c.requestStatus(ctx)
		}
	case proto.TRPCReq:
		go c.handleRPC(ctx, env)
	default:
		c.log.Debug("unhandled inbound", "t", env.T)
	}
}

// rpcReqBody mirrors the server's rpcRequest.
type rpcReqBody struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// rpcRespBody mirrors the server's rpcResponse.
type rpcRespBody struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (c *Conn) handleRPC(ctx context.Context, env proto.Envelope) {
	var req rpcReqBody
	if err := json.Unmarshal(env.D, &req); err != nil {
		_ = c.send(proto.TRPCResp, env.ID, rpcRespBody{Error: "bad request"})
		return
	}
	if req.Method == proto.RPCAgentUpgrade {
		c.handleUpgradeRPC(env.ID)
		return
	}
	result, err := c.handlers.OnRPC(ctx, req.Method, req.Params)
	resp := rpcRespBody{Result: result}
	if err != nil {
		resp = rpcRespBody{Error: err.Error()}
	}
	_ = c.send(proto.TRPCResp, env.ID, resp)
}

func (c *Conn) writeStreamHeader(h proto.StreamHeader) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.ctrl.Write(b)
	return err
}

// toWSURL converts an http(s) base URL to ws(s).
func toWSURL(u string) string {
	u = strings.TrimRight(u, "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	default:
		return u
	}
}
