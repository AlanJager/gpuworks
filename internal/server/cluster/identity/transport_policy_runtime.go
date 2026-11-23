package identity

import (
	"crypto/tls"
	"errors"
	"slices"
	"sync"
	"time"
)

// RuntimeTransportPolicy owns the durable policy lease until transport stops.
// Runtime changes may only add allowed keys. Removing keys, changing tombstones,
// or revoking established connections requires a separate stopped operation.
// Apply trusts the caller to authenticate and authorize the source snapshot.
type RuntimeTransportPolicy struct {
	// Publication and Close take publicationMu before mu. Disk I/O must not
	// hold mu: existing TLS peers use the last activated policy while an
	// additive publication waits for durable confirmation.
	publicationMu sync.Mutex
	mu            sync.Mutex
	path          string
	current       TransportPolicy
	authorize     PeerAuthorizer
	release       func() error
	closed        bool
	closeErr      error
	// confirmed says this owner has seen current's publication made durable:
	// a publication (or the directory sync that confirms an identical one)
	// succeeded since it opened. Guarded by publicationMu.
	confirmed bool
}

func OpenRuntimeTransportPolicy(path string) (*RuntimeTransportPolicy, error) {
	release, err := LockTransportPolicy(path)
	if err != nil {
		return nil, err
	}
	policy, err := LoadTransportPolicy(path)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	authorize, err := policy.Authorizer(policy.ClusterID, policy.CAFingerprint)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	return &RuntimeTransportPolicy{path: path, current: policy, authorize: authorize, release: release}, nil
}

// Authorize is safe for concurrent TLS handshakes and denies all keys after Close.
func (r *RuntimeTransportPolicy) Authorize(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime transport policy is closed")
	}
	return r.authorize(key)
}

// TLSConfigs binds both the pinned authority and local key before returning TLS
// configurations whose peer checks follow successful additive publications.
func (r *RuntimeTransportPolicy) TLSConfigs(trust *Trust, certificate, key []byte, now time.Time) (*tls.Config, *tls.Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || trust == nil {
		return nil, nil, errors.New("runtime policy requires open owner and pinned trust")
	}
	authorize, err := r.current.Authorizer(trust.clusterID, digest(trust.certificate.Raw))
	if err != nil {
		return nil, nil, err
	}
	local, err := parseCertificate(certificate)
	if err != nil {
		return nil, nil, err
	}
	fingerprint, err := trust.VerifyMember(local, now)
	if err != nil {
		return nil, nil, err
	}
	if err := authorize(fingerprint); err != nil {
		return nil, nil, errors.New("local member is not authorized by runtime transport policy")
	}
	return trust.TLSConfigs(certificate, key, now, r.Authorize)
}

func (r *RuntimeTransportPolicy) Apply(snapshot TransportPolicy) (TransportPolicy, error) {
	return r.apply(snapshot, applyTransportPolicySnapshotLocked)
}

func (r *RuntimeTransportPolicy) apply(snapshot TransportPolicy, publish func(string, TransportPolicy, uint64) (TransportPolicy, error)) (TransportPolicy, error) {
	r.publicationMu.Lock()
	defer r.publicationMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return TransportPolicy{}, errors.New("runtime transport policy is closed")
	}
	current := r.current
	r.mu.Unlock()
	// Only serialized publications replace current or its owned slices.
	if !slices.Equal(snapshot.RevokedKeys, current.RevokedKeys) {
		return TransportPolicy{}, errors.New("runtime transport update cannot change revocations")
	}
	for _, key := range current.AllowedKeys {
		if !slices.Contains(snapshot.AllowedKeys, key) {
			return TransportPolicy{}, errors.New("runtime transport update cannot remove allowed keys")
		}
	}
	// A running owner must never bootstrap a missing policy or forget a
	// previously observed local/source high-water mark. A higher durable state
	// can arise when a prior publication succeeded but its sync was unconfirmed.
	durable, err := LoadTransportPolicy(r.path)
	if err != nil {
		return TransportPolicy{}, err
	}
	if durable.ClusterID != current.ClusterID || durable.CAFingerprint != current.CAFingerprint || durable.Revision < current.Revision || durable.SourceRevision < current.SourceRevision || snapshot.SourceRevision < current.SourceRevision {
		return TransportPolicy{}, errors.New("runtime transport policy durable identity or revision regressed")
	}
	if durable.SourceRevision == current.SourceRevision && (!slices.Equal(durable.AllowedKeys, current.AllowedKeys) || !slices.Equal(durable.RevokedKeys, current.RevokedKeys)) {
		return TransportPolicy{}, errors.New("runtime transport policy source contents changed without advancing revision")
	}
	// The same snapshot again, over a file this owner already confirmed and
	// that still reads back as confirmed: nothing to write and nothing to
	// sync. The authorization refresh applies its snapshot every second, and
	// a directory sync per second waits on the disk for no change. The lease
	// keeps other publishers out, and the first Apply after opening still
	// syncs, which confirms a publication whose sync a previous owner left
	// unknown.
	if r.confirmed && sameTransportPolicy(durable, current) && snapshot.SourceRevision == current.SourceRevision && slices.Equal(snapshot.AllowedKeys, current.AllowedKeys) {
		current.AllowedKeys = slices.Clone(current.AllowedKeys)
		current.RevokedKeys = slices.Clone(current.RevokedKeys)
		return current, nil
	}
	candidate := snapshot
	candidate.Revision = 1
	authorize, err := candidate.Authorizer(current.ClusterID, current.CAFingerprint)
	if err != nil {
		return TransportPolicy{}, err
	}
	persisted, err := publish(r.path, snapshot, durable.Revision)
	if err != nil {
		return TransportPolicy{}, err
	}
	// Publication (including directory sync) must succeed before granting new
	// handshakes. Unknown durability leaves the previous in-memory policy intact.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = persisted
	r.current.AllowedKeys = slices.Clone(persisted.AllowedKeys)
	r.current.RevokedKeys = slices.Clone(persisted.RevokedKeys)
	r.authorize = authorize
	r.confirmed = true
	return persisted, nil
}

func sameTransportPolicy(a, b TransportPolicy) bool {
	return a.ClusterID == b.ClusterID && a.CAFingerprint == b.CAFingerprint && a.Revision == b.Revision && a.SourceRevision == b.SourceRevision &&
		slices.Equal(a.AllowedKeys, b.AllowedKeys) && slices.Equal(a.RevokedKeys, b.RevokedKeys)
}

// Close must run after listeners and connections have stopped. Repeated calls
// return the same release result and never unlock another owner's lease.
func (r *RuntimeTransportPolicy) Close() error {
	r.publicationMu.Lock()
	defer r.publicationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.closeErr = r.release()
	}
	return r.closeErr
}

// Snapshot reports only the last policy durably confirmed and activated in this
// owner. Reading the file instead could acknowledge an unconfirmed publication.
func (r *RuntimeTransportPolicy) Snapshot() (TransportPolicy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return TransportPolicy{}, errors.New("runtime transport policy is closed")
	}
	snapshot := r.current
	snapshot.AllowedKeys = slices.Clone(r.current.AllowedKeys)
	snapshot.RevokedKeys = slices.Clone(r.current.RevokedKeys)
	return snapshot, nil
}
