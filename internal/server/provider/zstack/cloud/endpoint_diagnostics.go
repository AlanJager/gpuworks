package cloud

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
)

type ipProbeResult string

const (
	ipResponded        ipProbeResult = "responded"
	ipNoReply          ipProbeResult = "no_reply"
	ipProbeUnavailable ipProbeResult = "unavailable"
)

type endpointDiagnosis struct {
	tcpError error
	ip       ipProbeResult
}

type endpointDiagnostics struct {
	timeout time.Duration
	tcp     func(context.Context, string) error
	ip      func(context.Context, string) ipProbeResult
}

func defaultEndpointDiagnostics() *endpointDiagnostics {
	// Advisory probes share a short budget, independent of the expired API
	// attempt, so they add at most two seconds to a failed validation.
	return &endpointDiagnostics{timeout: 2 * time.Second, tcp: probeTCP, ip: probeIP}
}

func needsEndpointDiagnosis(err error) bool {
	var apiErr *apiclient.APIError
	return errors.As(err, &apiErr) && apiErr.Phase == apiclient.PhaseConnect &&
		!apiErr.ViaProxy && !errors.Is(err, context.Canceled) && apiErr.Kind != apiclient.ErrorCanceled && connectionErrorCode(err) == ""
}

func (d *endpointDiagnostics) run(ctx context.Context, endpoint string) *endpointDiagnosis {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Hostname() == "" {
		return nil
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	result := &endpointDiagnosis{ip: ipProbeUnavailable}
	var probes sync.WaitGroup
	probes.Add(2)
	go func() {
		defer probes.Done()
		result.tcpError = d.tcp(ctx, net.JoinHostPort(u.Hostname(), port))
	}()
	go func() {
		defer probes.Done()
		result.ip = d.ip(ctx, u.Hostname())
	}()
	probes.Wait()
	return result
}

func probeTCP(ctx context.Context, address string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err == nil {
		_ = conn.Close()
	}
	return err
}

func probeIP(ctx context.Context, host string) ipProbeResult {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return ipProbeUnavailable
	}
	// Pass a resolved IP, never a user-controlled command option or shell text.
	cmd := exec.CommandContext(ctx, "ping", "-n", "-c", "1", addresses[0].String())
	if err := cmd.Start(); err != nil {
		return ipProbeUnavailable
	}
	err = cmd.Wait()
	if err == nil {
		return ipResponded
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ipNoReply
	}
	var exitErr *exec.ExitError
	if runtime.GOOS == "linux" && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return ipNoReply
	}
	return ipProbeUnavailable
}
