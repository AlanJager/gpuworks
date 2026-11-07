package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
)

// benchmarkHTTPClient protects server-origin external load just like probes.
// The one internal exception is the configured self gateway of a persisted
// managed target. An external URL can never opt into that exception.
func (a *apiHandlers) benchmarkHTTPClient(run *store.BenchmarkRun) *http.Client {
	// Supplying the guarded transport must not remove the per-request bound;
	// benchrun replaces this default with the run's configured timeout.
	client := &http.Client{Transport: a.outboundTransport(), Timeout: benchmeta.DefaultRequestTimeoutSeconds * time.Second}
	if !a.managedBenchmarkGateway(run) {
		return client
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client.Transport = transport
	// The self gateway has no redirects; following one would extend this
	// narrow trust decision to an arbitrary destination.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func (a *apiHandlers) managedBenchmarkGateway(run *store.BenchmarkRun) bool {
	if run.TargetKind != "instance" || run.DeploymentID == 0 || run.InstanceUUID == "" || run.LoadOrigin != "server" || a.d.Store == nil {
		return false
	}
	loopback := strings.TrimRight(strings.TrimSpace(a.serverSettings().LoopbackURL()), "/")
	parsed, err := url.Parse(loopback)
	if err != nil || parsed.User != nil || parsed.Scheme != "http" || !net.ParseIP(parsed.Hostname()).IsLoopback() || run.BaseURL != loopback+"/api/v1/playground" {
		return false
	}
	instance, err := a.d.Store.InstanceByUUID(run.InstanceUUID)
	if err != nil || instance.DeploymentID != run.DeploymentID {
		return false
	}
	deployment, err := a.d.Store.GetDeployment(run.DeploymentID)
	return err == nil && deployment.ServedName == run.Model
}
