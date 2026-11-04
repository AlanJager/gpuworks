package engine

import (
	"context"
	"errors"
	"net"
	"net/url"
	"syscall"
)

// Health URLs can contain query credentials. Only the requested endpoint's
// scheme, host and path belong in the startup diagnostic, never its body/query.
func healthDiagnosticTarget(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

func healthTransportResult(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	return "transport_error"
}

func healthDiagnosticDetail(target, result string) string {
	return "from=node requested_target=" + target + " result=" + result + "; readiness probe only; gateway and client path not tested"
}
