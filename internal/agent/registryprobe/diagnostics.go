package registryprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"syscall"
)

type probePathKey struct{}

// This wraps the resolution used by the transport, rather than precomputing
// one for diagnostics. Custom transports that do not resolve Proxy stay unknown.
func observeProxy(proxy func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		var target *url.URL
		var err error
		if proxy != nil {
			target, err = proxy(req)
		}
		if observed, ok := req.Context().Value(probePathKey{}).(*atomic.Int32); ok {
			kind := int32(1)
			if err != nil {
				kind = 0
			} else if target != nil {
				kind = 2
			}
			observed.Store(kind)
		}
		return target, err
	}
}

func probePath(kind int32) string {
	switch kind {
	case 1:
		return "direct"
	case 2:
		return "proxy"
	default:
		return "unknown"
	}
}

// Classifications are deliberately bounded. URL/transport errors may contain
// proxy credentials, so their raw text must not become a registry diagnostic.
func probeFailure(err error) string {
	if errors.Is(err, errOffline) {
		return "not_checked"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &verification) || errors.As(err, &record) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return "tls"
	}
	return "transport_error"
}
