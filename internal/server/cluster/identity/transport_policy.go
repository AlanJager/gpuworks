package identity

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/filelock"
)

const transportPolicyLimit = 128 << 10

// TransportPolicy is a durable transport authorization snapshot. External
// publishers require stopped transport. Its runtime owner may durably add
// allowed keys; removing authorization also requires draining old connections.
// Revoked keys are permanent tombstones and must never be reused.
type TransportPolicy struct {
	ClusterID      string   `json:"cluster_id"`
	CAFingerprint  string   `json:"ca_fingerprint"`
	Revision       uint64   `json:"revision"`
	SourceRevision uint64   `json:"source_revision,omitempty"`
	AllowedKeys    []string `json:"allowed_keys"`
	RevokedKeys    []string `json:"revoked_keys"`
}

func policyDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func (p TransportPolicy) validate() error {
	if !validID(p.ClusterID) || !policyDigest(p.CAFingerprint) || p.Revision == 0 || len(p.AllowedKeys) == 0 {
		return errors.New("invalid transport policy identity, revision or allowed keys")
	}
	allowed := make(map[string]bool, len(p.AllowedKeys))
	for _, keys := range [][]string{p.AllowedKeys, p.RevokedKeys} {
		for i, key := range keys {
			if !policyDigest(key) || (i > 0 && keys[i-1] >= key) {
				return errors.New("transport policy keys must be sorted unique lowercase SHA256 fingerprints")
			}
		}
	}
	for _, key := range p.AllowedKeys {
		allowed[key] = true
	}
	for _, key := range p.RevokedKeys {
		if allowed[key] {
			return errors.New("transport policy cannot authorize a revoked key")
		}
	}
	return nil
}

// Authorizer takes a defensive copy. Updating either the source object or its
// backing file cannot change a running transport's authorization decision.
func (p TransportPolicy) Authorizer(clusterID, pin string) (PeerAuthorizer, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if p.ClusterID != clusterID || p.CAFingerprint != pin {
		return nil, errors.New("transport policy cluster authority mismatch")
	}
	allowed := make(map[string]struct{}, len(p.AllowedKeys))
	for _, key := range p.AllowedKeys {
		allowed[key] = struct{}{}
	}
	return func(key string) error {
		if _, ok := allowed[key]; !ok {
			return errors.New("member key is not authorized for transport")
		}
		return nil
	}, nil
}

func policyDirectory(path string) error {
	if err := privateKeyDirectory(path); err != nil {
		return err
	}
	absolute, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	for {
		info, err := os.Lstat(absolute)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("transport policy directory must not contain symlinks")
		}
		parent := filepath.Dir(absolute)
		if parent == absolute {
			break
		}
		absolute = parent
	}
	return nil
}

func policyFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("transport policy requires a private regular file")
	}
	return info, nil
}

// LoadTransportPolicy fails closed; malformed or missing state is never repaired.
func LoadTransportPolicy(path string) (TransportPolicy, error) {
	var p TransportPolicy
	if err := policyDirectory(path); err != nil {
		return p, err
	}
	info, err := policyFileInfo(path)
	if err != nil {
		return p, err
	}
	if info.Size() <= 0 || info.Size() > transportPolicyLimit {
		return p, errors.New("transport policy file size is invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return p, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return p, err
	}
	if !os.SameFile(info, actual) {
		return p, errors.New("transport policy changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, transportPolicyLimit+1))
	if err != nil {
		return p, err
	}
	if len(data) > transportPolicyLimit {
		return p, errors.New("transport policy exceeds size limit")
	}
	// Reject duplicate keys as well as unknown fields and trailing JSON.
	tokens := json.NewDecoder(bytes.NewReader(data))
	token, err := tokens.Token()
	if err != nil || token != json.Delim('{') {
		return p, errors.New("transport policy must be a JSON object")
	}
	seen := map[string]bool{}
	for tokens.More() {
		field, err := tokens.Token()
		if err != nil {
			return p, err
		}
		name, ok := field.(string)
		if !ok || seen[name] {
			return p, errors.New("duplicate transport policy field")
		}
		switch name {
		case "cluster_id", "ca_fingerprint", "revision", "allowed_keys", "revoked_keys", "source_revision":
		default:
			return p, errors.New("unknown transport policy field")
		}
		seen[name] = true
		var raw json.RawMessage
		if err := tokens.Decode(&raw); err != nil {
			return p, err
		}
		if name == "source_revision" {
			var revision uint64
			if err := json.Unmarshal(raw, &revision); err != nil || revision == 0 {
				return p, errors.New("transport source revision must be a positive integer")
			}
		}
	}
	if !seen["cluster_id"] || !seen["ca_fingerprint"] || !seen["revision"] || !seen["allowed_keys"] || !seen["revoked_keys"] {
		return p, errors.New("transport policy fields are incomplete")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return TransportPolicy{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return TransportPolicy{}, errors.New("trailing transport policy data")
	}
	if err := p.validate(); err != nil {
		return TransportPolicy{}, err
	}
	return p, nil
}

// syncPolicyDirectory makes the policy file's directory entry durable. It is
// a variable so a test can count the syncs.
var syncPolicyDirectory = func(path string) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func acquireTransportPolicyLock(path string) (func() error, error) {
	if err := policyDirectory(path); err != nil {
		return nil, err
	}
	lockPath := path + ".lock"
	if _, err := policyFileInfo(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return filelock.Acquire(lockPath)
}

// LockTransportPolicy prevents policy publication for a transport's lifetime.
// The caller must load its authorization snapshot after taking this lock, and
// release it only after all transport listeners and connections have stopped.
// Invalid or missing policy state is rejected without leaving a lease held.
func LockTransportPolicy(path string) (func() error, error) {
	if _, err := LoadTransportPolicy(path); err != nil {
		return nil, err
	}
	release, err := acquireTransportPolicyLock(path)
	if err != nil {
		return nil, err
	}
	if _, err := LoadTransportPolicy(path); err != nil {
		return nil, errors.Join(err, release())
	}
	return release, nil
}

// PublishTransportPolicy performs a durable compare-and-swap. Repeating an
// identical publication confirms an uncertain prior directory sync. A busy lock
// is returned to the caller; it never silently waits or merges policy changes.
func PublishTransportPolicy(path string, expectedRevision uint64, next TransportPolicy) (resultErr error) {
	if err := next.validate(); err != nil {
		return err
	}
	if expectedRevision == ^uint64(0) || next.Revision != expectedRevision+1 {
		return errors.New("transport policy revision must advance by one")
	}
	release, err := acquireTransportPolicyLock(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	return publishTransportPolicyLocked(path, expectedRevision, next)
}

// publishTransportPolicyLocked requires the caller to own the policy lock.
func publishTransportPolicyLocked(path string, expectedRevision uint64, next TransportPolicy) error {
	current, err := LoadTransportPolicy(path)
	if err != nil && !(expectedRevision == 0 && errors.Is(err, os.ErrNotExist)) {
		return err
	}
	if err == nil {
		if reflect.DeepEqual(current, next) {
			return syncPolicyDirectory(path)
		}
		if current.Revision != expectedRevision || expectedRevision == 0 {
			return errors.New("transport policy revision conflict")
		}
		if current.ClusterID != next.ClusterID || current.CAFingerprint != next.CAFingerprint {
			return errors.New("transport policy authority cannot change")
		}
		if next.SourceRevision < current.SourceRevision {
			return errors.New("transport policy source revision cannot decrease")
		}
		if current.SourceRevision != 0 && next.SourceRevision == current.SourceRevision && (!slices.Equal(current.AllowedKeys, next.AllowedKeys) || !slices.Equal(current.RevokedKeys, next.RevokedKeys)) {
			return errors.New("same transport source revision cannot change authorized keys")
		}
		revoked := make(map[string]bool, len(next.RevokedKeys))
		for _, key := range next.RevokedKeys {
			revoked[key] = true
		}
		allowed := make(map[string]bool, len(next.AllowedKeys))
		for _, key := range next.AllowedKeys {
			allowed[key] = true
		}
		for _, key := range current.RevokedKeys {
			if !revoked[key] {
				return errors.New("transport policy cannot discard revocation tombstones")
			}
		}
		for _, key := range current.AllowedKeys {
			if !allowed[key] && !revoked[key] {
				return errors.New("removed transport keys must be revoked")
			}
		}
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(data) > transportPolicyLimit {
		return errors.New("transport policy exceeds size limit")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".transport-policy-")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	_, writeErr := temporary.Write(data)
	if err := errors.Join(writeErr, temporary.Sync(), temporary.Close()); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	if err := syncPolicyDirectory(path); err != nil {
		return fmt.Errorf("transport policy published but durability unknown: %w", err)
	}
	return nil
}

// ApplyTransportPolicySnapshot publishes an already authenticated source snapshot
// while transport is stopped. SourceRevision orders source authorization; the
// local Revision remains a separate consecutive CAS counter. This function does
// not fetch, authenticate, or prove quorum for the supplied snapshot.
func ApplyTransportPolicySnapshot(path string, snapshot TransportPolicy) (result TransportPolicy, resultErr error) {
	if snapshot.Revision != 0 || snapshot.SourceRevision == 0 {
		return result, errors.New("source snapshot requires zero local revision and positive source revision")
	}
	snapshot.Revision = 1
	if err := snapshot.validate(); err != nil {
		return result, err
	}
	release, err := acquireTransportPolicyLock(path)
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, release())
		if resultErr != nil {
			result = TransportPolicy{}
		}
	}()
	snapshot.Revision = 0
	return applyTransportPolicySnapshotLocked(path, snapshot, 0)
}

func applyTransportPolicySnapshotLocked(path string, snapshot TransportPolicy, minimumRevision uint64) (result TransportPolicy, resultErr error) {
	if snapshot.Revision != 0 || snapshot.SourceRevision == 0 {
		return result, errors.New("source snapshot requires zero local revision and positive source revision")
	}
	snapshot.Revision = 1
	if err := snapshot.validate(); err != nil {
		return result, err
	}
	current, err := LoadTransportPolicy(path)
	if minimumRevision > 0 && (err != nil || current.Revision < minimumRevision) {
		return result, errors.Join(errors.New("existing transport policy revision is missing or regressed"), err)
	}
	expected := uint64(0)
	if err == nil {
		if current.ClusterID != snapshot.ClusterID || current.CAFingerprint != snapshot.CAFingerprint {
			return result, errors.New("transport policy authority cannot change")
		}
		if current.SourceRevision == snapshot.SourceRevision && slices.Equal(current.AllowedKeys, snapshot.AllowedKeys) && slices.Equal(current.RevokedKeys, snapshot.RevokedKeys) {
			if err := syncPolicyDirectory(path); err != nil {
				return result, err
			}
			return current, nil
		}
		if current.Revision == ^uint64(0) {
			return result, errors.New("transport policy local revision exhausted")
		}
		expected = current.Revision
		snapshot.Revision = current.Revision + 1
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if err := publishTransportPolicyLocked(path, expected, snapshot); err != nil {
		return result, err
	}
	return snapshot, nil
}
