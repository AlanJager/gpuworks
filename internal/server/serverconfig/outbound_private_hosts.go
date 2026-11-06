package serverconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// maxOutboundConfigBytes bounds work done for one runtime policy read.
const maxOutboundConfigBytes = 1 << 20

// WatchOutboundPrivateHosts reads the administrator's current private HTTP
// exceptions and restriction mode. Ordinary private targets are unrestricted
// unless the configuration explicitly enables outbound_restrict_private_hosts;
// special targets remain forbidden by the outbound transport in either mode.
// A missing, invalid or unreadable file never falls back to startup permissions.
// With no file, the supplied mode and exceptions remain immutable.
//
// Each call reads the file, so revocation is observed even if an editor keeps
// its inode, timestamps and length. Only changed content is reparsed. Runtime
// reads are bounded to 1 MiB; larger files revoke exceptions without changing
// Load's startup contract. Results are copied so callers cannot mutate cached
// authority. report runs outside the lock on entering a different error state.
func WatchOutboundPrivateHosts(path string, restrict bool, fallback []string, report func(error)) func() []string {
	if path == "" {
		initial := effectiveOutboundPrivateHosts(restrict, fallback)
		return func() []string { return append([]string(nil), initial...) }
	}
	var mu sync.Mutex
	var cached []byte
	var hosts []string
	var lastError string
	return func() []string {
		mu.Lock()
		result, err := func() (result []string, err error) {
			f, err := openOutboundConfiguration(path)
			if err != nil {
				return nil, err
			}
			defer func() {
				err = errors.Join(err, f.Close())
				if err != nil {
					result = nil
				}
			}()
			before, err := f.Stat()
			if err != nil {
				return nil, err
			}
			if !before.Mode().IsRegular() {
				return nil, fmt.Errorf("read outbound configuration %s: expected a regular file", path)
			}
			if before.Size() > maxOutboundConfigBytes {
				return nil, fmt.Errorf("read outbound configuration %s: file exceeds %d bytes", path, maxOutboundConfigBytes)
			}
			raw, err := io.ReadAll(io.LimitReader(f, maxOutboundConfigBytes+1))
			if err != nil {
				return nil, err
			}
			if len(raw) > maxOutboundConfigBytes {
				return nil, fmt.Errorf("read outbound configuration %s: file exceeds %d bytes", path, maxOutboundConfigBytes)
			}
			nextHosts := hosts
			if cached == nil || !bytes.Equal(cached, raw) {
				cfg, err := decodeFile(path, raw)
				if err != nil {
					return nil, err
				}
				nextHosts = effectiveOutboundPrivateHosts(cfg.OutboundRestrictPrivateHosts, cfg.OutboundAllowPrivateHosts)
			}
			after, err := f.Stat()
			if err != nil {
				return nil, err
			}
			if !sameOutboundConfigSnapshot(before, after) {
				return nil, fmt.Errorf("outbound configuration %s changed while being read", path)
			}
			current, err := os.Stat(path)
			if err != nil {
				return nil, err
			}
			if !sameOutboundConfigSnapshot(before, current) {
				return nil, fmt.Errorf("outbound configuration %s changed while being read", path)
			}
			cached, hosts = raw, nextHosts
			return append([]string(nil), hosts...), nil
		}()
		currentError := ""
		if err != nil {
			cached, hosts = nil, nil
			currentError = err.Error()
		}
		changed := currentError != "" && currentError != lastError
		lastError = currentError
		mu.Unlock()
		if changed && report != nil {
			report(err)
		}
		return result
	}
}

func sameOutboundConfigSnapshot(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size() && a.Mode() == b.Mode()
}

// Broad IP exceptions express the default mode through the existing transport
// policy. Its permanent special-address checks run before these exceptions.
func effectiveOutboundPrivateHosts(restrict bool, hosts []string) []string {
	result := append([]string(nil), hosts...)
	if !restrict {
		result = append(result, "0.0.0.0/0", "::/0")
	}
	return result
}
