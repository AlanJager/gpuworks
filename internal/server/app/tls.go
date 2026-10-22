package app

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// listenerTLS loads certificates before any listener or background task is
// started. Explicit addresses let the loopback maintenance sockets continue
// serving the install, upgrade and benchmark paths without a public certificate.
func (c Config) listenerTLS() (*tls.Config, error) {
	if c.TLSCertFile == "" && c.TLSKeyFile == "" {
		return nil, nil
	}
	if c.TLSCertFile == "" || c.TLSKeyFile == "" {
		return nil, errors.New("tls_cert_file and tls_key_file must both be set")
	}
	if c.Listen.Empty() {
		return nil, errors.New("native TLS requires explicit listen.console addresses in server.yaml; --addr does not create the local maintenance socket")
	}
	public := false
	for _, binding := range c.bindings() {
		host, _, err := net.SplitHostPort(binding.Addr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || ip.IsUnspecified() {
			return nil, errors.New("native TLS requires explicit listen IPs; wildcard addresses cannot separate the local maintenance socket")
		}
		public = public || !serverconfig.ServesEverything(ip)
	}
	if !public {
		return nil, errors.New("native TLS needs a console address other than the loopback maintenance address")
	}
	cert, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server TLS certificate: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
}

func publicTLSListener(ln net.Listener) bool {
	addr, ok := ln.Addr().(*net.TCPAddr)
	return ok && !serverconfig.ServesEverything(addr.IP)
}
