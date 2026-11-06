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
// exceptions. A file-backed configuration never falls back to startup grants:
// missing, invalid or unreadable files revoke those exceptions until recovery.
// With no configuration file, explicit in-process settings remain immutable.
//
// Each call reads the file, so revocation is observed even if an editor keeps
// its inode, timestamps and length. Only changed content is reparsed. Runtime
// reads are bounded to 1 MiB; larger files revoke exceptions without changing
// Load's startup contract. Results are copied so callers cannot mutate cached
// authority. report runs outside the lock on entering a different error state.
func WatchOutboundPrivateHosts(path string, fallback []string, report func(error)) func() []string {
	if path == "" {
		initial := append([]string(nil), fallback...)
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
				nextHosts = append([]string(nil), cfg.OutboundAllowPrivateHosts...)
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
