package compshare

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// HostKeyStore is the minimal persistence boundary for provider-scoped TOFU.
type HostKeyStore interface {
	Get(endpoint string) (string, bool)
	Put(endpoint, fingerprint string) error
}

type memoryHostKeys struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemoryHostKeys() *memoryHostKeys { return &memoryHostKeys{m: map[string]string{}} }

type fileHostKeys struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

func newFileHostKeys(path string) (*fileHostKeys, error) {
	s := &fileHostKeys{path: path, m: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s.m); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}
func (s *fileHostKeys) Get(endpoint string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[endpoint]
	return v, ok
}
func (s *fileHostKeys) Put(endpoint, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[endpoint] = fingerprint
	b, err := json.Marshal(s.m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
func (s *memoryHostKeys) Get(endpoint string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[endpoint]
	return v, ok
}
func (s *memoryHostKeys) Put(endpoint, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[endpoint] = fingerprint
	return nil
}

func hostKeyFingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + strings.TrimRight(base64.RawStdEncoding.EncodeToString(sum[:]), "=")
}

func endpointKey(host string, port int) string {
	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}

// tofuCallback records the first key and rejects changes thereafter. The store
// must be persistent in production; the memory implementation is for tests.
func tofuCallback(store HostKeyStore, endpoint string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := hostKeyFingerprint(key)
		if want, ok := store.Get(endpoint); ok {
			if want != got {
				return fmt.Errorf("ssh host key mismatch for %s", endpoint)
			}
			return nil
		}
		return store.Put(endpoint, got)
	}
}
