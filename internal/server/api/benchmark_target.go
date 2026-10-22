package api

import (
	"fmt"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Benchmark entries, recorded in the target snapshot so a result says what
// it measured (plan D9, R19).
const (
	// benchmarkEntryGateway measures the deployment the way clients reach it:
	// the /v1 gateway, from the server, with its load balancing and, for a PD
	// deployment, its two-stage handoff.
	benchmarkEntryGateway = "gateway"
	// benchmarkEntryInstance measures one instance directly from its node. On
	// a PD deployment that is not an end-to-end result: a prefill instance
	// answers like an ordinary engine and a decode instance computes the
	// prefill itself.
	benchmarkEntryInstance = "instance"
)

// managedBenchmarkTarget is where a benchmark of a managed deployment sends
// its load, and what the snapshot records about it.
type managedBenchmarkTarget struct {
	deployment   *store.Deployment
	instances    []store.Instance
	instance     *store.Instance // the instance the snapshot describes
	node         *store.Node     // that instance's node
	baseURL      string
	model        string
	origin       string
	apiKey       string
	entry        string
	numGPUs      int
	instanceUUID string // the instance measured, or the one describing a gateway run
}

type benchmarkTargetError struct {
	status   int
	code     string
	fallback string
}

func (e *benchmarkTargetError) write(w http.ResponseWriter) {
	writeProductError(w, e.status, e.code, e.fallback, nil)
}

// resolveManagedBenchmarkTarget picks the entry for a managed deployment. No
// instance named: the gateway from the server, unless a node origin asks for
// the direct path of an instance on that node. An instance named: that
// instance, directly, from its own node — the only place its port is reachable
// without the gateway.
func (a *apiHandlers) resolveManagedBenchmarkTarget(r *http.Request, deploymentID uint, instanceUUID, origin string) (managedBenchmarkTarget, *benchmarkTargetError) {
	dep, err := a.d.Store.GetDeployment(deploymentID)
	if err != nil || dep.State != store.DeployActive {
		return managedBenchmarkTarget{}, &benchmarkTargetError{http.StatusBadRequest, ErrorBenchmarkTargetInactive, "target deployment is not active"}
	}
	insts, _ := a.d.Store.InstancesForDeployment(dep.ID)
	target := managedBenchmarkTarget{deployment: dep, instances: insts, model: dep.ServedName}
	serving := func(inst *store.Instance) bool {
		return inst.State == proto.InstRunning && inst.SleepLevel == 0 && !inst.Headless && inst.APIPort > 0
	}
	for i := range insts {
		if insts[i].State == proto.InstRunning && insts[i].SleepLevel == 0 {
			target.numGPUs += instanceGPUCount(insts[i])
		}
	}
	instanceUUID, origin = strings.TrimSpace(instanceUUID), strings.TrimSpace(origin)
	if instanceUUID != "" {
		for i := range insts {
			if insts[i].UUID == instanceUUID && serving(&insts[i]) {
				target.instance = &insts[i]
			}
		}
		if target.instance == nil {
			return target, &benchmarkTargetError{http.StatusConflict, ErrorBenchmarkLeaderNotRunning, "the named instance is not awake and running"}
		}
		target.node, _ = a.d.Store.NodeByID(target.instance.NodeID)
		if target.node == nil || (origin != "" && origin != "auto" && origin != target.node.UUID) {
			return target, &benchmarkTargetError{http.StatusBadRequest, ErrorBenchmarkInstanceOrigin, "a single instance is measured from its own node"}
		}
		target.origin, target.entry, target.instanceUUID = target.node.UUID, benchmarkEntryInstance, instanceUUID
		target.baseURL = fmt.Sprintf("http://127.0.0.1:%d", target.instance.APIPort)
		target.numGPUs = instanceGPUCount(*target.instance)
		return target, nil
	}
	if origin != "" && origin != "auto" && origin != "server" {
		// A node origin keeps the direct path it always had: an instance on
		// that node, the decode role on a PD deployment.
		for i := range insts {
			if !serving(&insts[i]) || insts[i].NodeRank != 0 || (insts[i].Role != "" && insts[i].Role != pdmode.RoleDecode) {
				continue
			}
			if node, err := a.d.Store.NodeByID(insts[i].NodeID); err == nil && node.UUID == origin {
				target.instance, target.node = &insts[i], node
				break
			}
		}
		if target.instance == nil {
			return target, &benchmarkTargetError{http.StatusConflict, ErrorBenchmarkLeaderNotRunning, "no instance of the target deployment is awake and running on the load origin node"}
		}
		target.origin, target.entry, target.instanceUUID = origin, benchmarkEntryInstance, target.instance.UUID
		target.baseURL = fmt.Sprintf("http://127.0.0.1:%d", target.instance.APIPort)
		return target, nil
	}
	for i := range insts {
		if serving(&insts[i]) && insts[i].NodeRank == 0 && (insts[i].Role == "" || insts[i].Role == pdmode.RoleDecode) {
			target.instance = &insts[i]
			break
		}
	}
	if target.instance == nil {
		return target, &benchmarkTargetError{http.StatusConflict, ErrorBenchmarkLeaderNotRunning, "target deployment leader is not awake and running"}
	}
	target.node, _ = a.d.Store.NodeByID(target.instance.NodeID)
	claims, ok := a.claimsFromRequest(r)
	if !ok {
		return target, &benchmarkTargetError{http.StatusUnauthorized, ErrorAuthenticationRequired, "authentication required"}
	}
	token := auth.BearerToken(r.Header.Get("Authorization"))
	if !auth.IsPlatformToken(token) {
		token, err = a.d.AuthSigner.Sign(claims)
		if err != nil {
			return target, &benchmarkTargetError{http.StatusInternalServerError, ErrorBenchmarkServerOriginUnavailable, err.Error()}
		}
	}
	// The server calls its own authenticated internal entry to the /v1
	// gateway over loopback, so no address it is known by elsewhere matters.
	loopback := strings.TrimRight(strings.TrimSpace(a.serverSettings().LoopbackURL()), "/")
	if loopback == "" {
		return target, &benchmarkTargetError{http.StatusServiceUnavailable, ErrorBenchmarkServerOriginUnavailable, "the server binds no port it can reach itself on"}
	}
	target.origin, target.entry, target.instanceUUID = "server", benchmarkEntryGateway, target.instance.UUID
	target.baseURL, target.apiKey = loopback+"/api/v1/playground", token
	return target, nil
}
