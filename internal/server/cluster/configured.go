package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

type transportDescriptor struct {
	ClusterID     string `json:"cluster_id"`
	CAFingerprint string `json:"ca_fingerprint"`
	Address       string `json:"address"`
}

// PrepareDatabaseTransport installs an immutable member identity. The caller
// must hold the database runtime lease. Authorization updates use the policy
// publication API while the database is stopped, not this initialization API.
func PrepareDatabaseTransport(dir, address, clusterID, pin string, ca, certificate, key []byte, policy identity.TransportPolicy) error {
	if err := transportDirectory(dir); err != nil {
		return err
	}
	descriptor := transportDescriptor{clusterID, pin, address}
	config, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	if policy.Revision != 1 {
		return errors.New("initial transport policy revision must be one")
	}
	if _, err := validateTransport(descriptor, ca, certificate, key, policy); err != nil {
		return err
	}
	files := map[string][]byte{"config.json": config, "ca.pem": ca, "member.pem": certificate, "member.key": key}
	// The marker binds the immutable identity, while policy revisions remain
	// independently replaceable using their own compare-and-swap protocol.
	hash := sha256.New()
	for _, name := range []string{"config.json", "ca.pem", "member.pem", "member.key"} {
		fmt.Fprintf(hash, "%d:", len(files[name]))
		_, _ = hash.Write(files[name])
	}
	marker := []byte(hex.EncodeToString(hash.Sum(nil)) + "\n")
	markerPath := filepath.Join(dir, ".transport-required")
	bundle := filepath.Join(dir, ".transport")
	existing, err := transportRead(markerPath)
	if err == nil {
		if !bytes.Equal(existing, marker) {
			return errors.New("transport initialization identity differs from durable intent")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		if _, err := os.Lstat(bundle); !errors.Is(err, os.ErrNotExist) {
			return errors.New("transport bundle exists without its required marker")
		}
		// Publish a complete marker without overwriting any concurrent intent.
		intent, err := os.CreateTemp(dir, ".transport-intent-")
		if err != nil {
			return err
		}
		intentPath := intent.Name()
		defer os.Remove(intentPath)
		_, writeErr := intent.Write(marker)
		if err := errors.Join(writeErr, intent.Sync(), intent.Close()); err != nil {
			return err
		}
		if err := os.Link(intentPath, markerPath); err != nil {
			return err
		}
		if err := transportSync(dir); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(bundle); err == nil {
		if err := transportDirectory(bundle); err != nil {
			return err
		}
		for name, data := range files {
			actual, err := transportRead(filepath.Join(bundle, name))
			if err != nil {
				return err
			}
			if !bytes.Equal(actual, data) {
				return errors.New("existing transport bundle differs; explicit recovery required")
			}
		}
		actual, err := identity.LoadTransportPolicy(filepath.Join(bundle, "policy.json"))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, policy) {
			return errors.New("existing transport policy differs")
		}
		return transportSync(dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp(dir, ".transport-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for name, data := range files {
		if err := transportWrite(filepath.Join(stage, name), data); err != nil {
			return err
		}
	}
	if err := identity.PublishTransportPolicy(filepath.Join(stage, "policy.json"), 0, policy); err != nil {
		return err
	}
	if err := transportSync(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, bundle); err != nil {
		return err
	}
	return transportSync(dir)
}

// OpenConfiguredDatabase never falls back to plaintext after transport setup
// begins. Application startup remains restricted to an established single node.
func OpenConfiguredDatabase(ctx context.Context, dir, name string) (*Database, error) {
	return openConfiguredDatabase(ctx, dir, name, false, true)
}

// OpenEstablishedMemberDatabase reopens a previously enrolled member of an
// established multi-member cluster. The caller must hold its runtime lease.
// It never initializes a new cluster or admits a single-member fallback.
func OpenEstablishedMemberDatabase(ctx context.Context, dir, name string) (*Database, error) {
	return openConfiguredDatabase(ctx, dir, name, true, true)
}

// StartEstablishedMemberDatabase starts a previously enrolled multi-member
// database after validating its durable identity. The caller holds its runtime
// lease and owns Close; no readiness or live membership is inferred here.
func StartEstablishedMemberDatabase(ctx context.Context, dir, name string) (*Database, error) {
	return openConfiguredDatabase(ctx, dir, name, true, false)
}

func openConfiguredDatabase(ctx context.Context, dir, name string, multiMember, wait bool) (*Database, error) {
	markerPath, bundle := filepath.Join(dir, ".transport-required"), filepath.Join(dir, ".transport")
	_, markerErr := os.Lstat(markerPath)
	_, bundleErr := os.Lstat(bundle)
	if errors.Is(markerErr, os.ErrNotExist) && errors.Is(bundleErr, os.ErrNotExist) {
		if multiMember {
			return nil, errors.New("established member requires durable transport identity")
		}
		return OpenSingleDatabase(ctx, dir, name)
	}
	if markerErr != nil {
		return nil, fmt.Errorf("transport marker: %w", markerErr)
	}
	if bundleErr != nil {
		return nil, fmt.Errorf("transport bundle: %w", bundleErr)
	}
	if err := transportDirectory(dir); err != nil {
		return nil, err
	}
	if err := transportDirectory(bundle); err != nil {
		return nil, err
	}
	marker, err := transportRead(markerPath)
	if err != nil {
		return nil, err
	}
	decoded, err := hex.DecodeString(string(bytes.TrimSuffix(marker, []byte("\n"))))
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("invalid required transport marker")
	}
	files := make(map[string][]byte)
	hash := sha256.New()
	for _, filename := range []string{"config.json", "ca.pem", "member.pem", "member.key"} {
		data, err := transportRead(filepath.Join(bundle, filename))
		if err != nil {
			return nil, err
		}
		files[filename] = data
		fmt.Fprintf(hash, "%d:", len(data))
		_, _ = hash.Write(data)
	}
	if !bytes.Equal(hash.Sum(nil), decoded) {
		return nil, errors.New("transport bundle does not match durable identity")
	}
	var descriptor transportDescriptor
	decoder := json.NewDecoder(bytes.NewReader(files["config.json"]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("transport descriptor has trailing content")
	}
	policyPath := filepath.Join(bundle, "policy.json")
	policy, err := identity.LoadTransportPolicy(policyPath)
	if err != nil {
		return nil, err
	}
	trust, err := validateTransport(descriptor, files["ca.pem"], files["member.pem"], files["member.key"], policy)
	if err != nil {
		return nil, err
	}
	var info struct {
		ID      uint64 `yaml:"ID"`
		Address string `yaml:"Address"`
	}
	raw, err := transportRead(filepath.Join(dir, "info.yaml"))
	if err != nil {
		return nil, fmt.Errorf("configured transport requires existing Raft identity: %w", err)
	}
	if err := yaml.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	var members []struct {
		ID      uint64 `yaml:"ID"`
		Address string `yaml:"Address"`
	}
	raw, err = transportRead(filepath.Join(dir, "cluster.yaml"))
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	if info.ID == 0 || info.Address != descriptor.Address || (!multiMember && len(members) != 1) || (multiMember && len(members) < 2) {
		return nil, errors.New("configured database has no established matching membership")
	}
	localPersisted := 0
	seenIDs, seenAddresses := make(map[uint64]bool), make(map[string]bool)
	for _, member := range members {
		if multiMember {
			if member.ID == 0 || member.Address == "" || seenIDs[member.ID] || seenAddresses[member.Address] {
				return nil, errors.New("configured database has invalid or duplicate persisted membership")
			}
			seenIDs[member.ID], seenAddresses[member.Address] = true, true
		}
		if member.ID == info.ID && member.Address == info.Address {
			localPersisted++
		} else if member.ID == 0 && member.Address == info.Address && !multiMember {
			localPersisted++
		}
	}
	if localPersisted != 1 {
		return nil, errors.New("configured database has no unique matching persisted member")
	}
	if _, err := os.Lstat(filepath.Join(dir, "join")); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("configured database has incomplete join state")
	}
	db, err := StartAuthorizedDatabase(ctx, DatabaseConfig{Dir: dir, Address: descriptor.Address, Name: name}, policyPath, trust, files["member.pem"], files["member.key"])
	if err != nil {
		return nil, err
	}
	if db.ID != info.ID || db.Address != info.Address {
		return nil, errors.Join(errors.New("configured database started with a different persisted identity"), db.Close())
	}
	if !wait {
		return db, nil
	}
	if err := db.WaitReady(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	live, err := db.Members(ctx)
	localLive := 0
	for _, member := range live {
		if member.ID == db.ID && member.Address == descriptor.Address {
			localLive++
		}
	}
	if err != nil || db.ID != info.ID || localLive != 1 || (!multiMember && len(live) != 1) || (multiMember && len(live) < 2) {
		if err == nil {
			err = errors.New("configured application database has no matching live membership")
		}
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func validateTransport(d transportDescriptor, ca, certificate, key []byte, policy identity.TransportPolicy) (*identity.Trust, error) {
	now := time.Now().UTC()
	trust, err := identity.NewTrust(ca, d.ClusterID, d.CAFingerprint, now)
	if err != nil {
		return nil, err
	}
	authorize, err := policy.Authorizer(d.ClusterID, d.CAFingerprint)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(certificate)
	if block == nil {
		return nil, errors.New("invalid member certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	fingerprint, err := trust.VerifyMember(cert, now)
	if err != nil {
		return nil, err
	}
	if err := authorize(fingerprint); err != nil {
		return nil, err
	}
	listen, dial, err := trust.TLSConfigs(certificate, key, now, authorize)
	if err != nil {
		return nil, err
	}
	if err := (DatabaseConfig{Dir: "configured", Name: "database", Address: d.Address, ListenTLS: listen, DialTLS: dial}).validate(); err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(d.Address)
	if err := cert.VerifyHostname(host); err != nil {
		return nil, err
	}
	return trust, nil
}

func transportDirectory(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for current := absolute; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("transport directory must not contain symlinks")
		}
		if current == absolute && info.Mode().Perm()&0077 != 0 {
			return errors.New("transport directory must be private")
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}
func transportRead(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 128<<10 {
		return nil, fmt.Errorf("transport file must be private, regular and bounded: %s", filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, actual) {
		return nil, errors.New("transport file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 128<<10 {
		return nil, errors.New("transport file too large")
	}
	return data, nil
}
func transportWrite(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	return errors.Join(writeErr, syncErr, f.Close())
}
func transportSync(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// TransportIdentity returns the pinned identity selected before database startup.
// Empty values identify the legacy private loopback transport.
func (d *Database) TransportIdentity() (string, string) {
	return d.transportClusterID, d.transportCAFingerprint
}
