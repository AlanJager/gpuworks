package identity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/filelock"
)

func TestRuntimeTransportPolicyAddsAuthenticatedTLSMember(t *testing.T) {
	fixture := newIdentityFixture(t)
	serverCert, serverKey, serverFP := fixture.member(t, "127.0.0.1:9010")
	clientCert, clientKey, clientFP := fixture.member(t, "127.0.0.1:9011")
	_, _, unknownFP := fixture.member(t, "127.0.0.1:9012")
	path, policy := policyFixture(t)
	pin, err := CertificateFingerprint(fixture.ca)
	if err != nil {
		t.Fatal(err)
	}
	policy.ClusterID = fixture.cluster
	policy.CAFingerprint = pin
	policy.AllowedKeys = []string{serverFP}
	policy.SourceRevision = 1
	if err := PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenRuntimeTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	server, _, err := owner.TLSConfigs(fixture.trust, serverCert, serverKey, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := owner.TLSConfigs(fixture.trust, clientCert, clientKey, fixture.now); err == nil {
		t.Fatal("unauthorized local key received TLS configurations")
	}
	_, client, err := fixture.trust.TLSConfigs(clientCert, clientKey, fixture.now, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, serverErr := exchangeTLS(t, server, client); serverErr == nil {
		t.Fatal("unadmitted member connected")
	}
	snapshot := policy
	snapshot.Revision = 0
	snapshot.SourceRevision = 2
	snapshot.AllowedKeys = []string{serverFP, clientFP}
	slices.Sort(snapshot.AllowedKeys)
	if _, err := ApplyTransportPolicySnapshot(path, snapshot); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("external apply bypassed runtime lease: %v", err)
	}
	raw := snapshot
	raw.Revision = 2
	if err := PublishTransportPolicy(path, 1, raw); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("external publish bypassed runtime lease: %v", err)
	}
	// A real filesystem read failure must not authorize the additional key.
	saved := path + ".saved"
	if err := os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if result, err := owner.Apply(snapshot); err == nil || !reflect.DeepEqual(result, TransportPolicy{}) {
		t.Fatalf("failed publication %#v %v", result, err)
	}
	if owner.Authorize(clientFP) == nil {
		t.Fatal("failed publication expanded memory permissions")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}
	applied, err := owner.Apply(snapshot)
	if err != nil || applied.Revision != 2 || applied.SourceRevision != 2 {
		t.Fatalf("additive publication %#v %v", applied, err)
	}
	// Reuse the exact existing TLS configuration, without recreating its callback.
	if clientErr, serverErr := exchangeTLS(t, server, client); clientErr != nil || serverErr != nil {
		t.Fatalf("new member refused after durable update: %v %v", clientErr, serverErr)
	}
	persisted, err := LoadTransportPolicy(path)
	if err != nil || !reflect.DeepEqual(persisted, applied) {
		t.Fatalf("published state %#v %v", persisted, err)
	}
	// Callers must not be able to mutate the owner's retained allowlist.
	applied.AllowedKeys[0] = unknownFP
	snapshot.AllowedKeys = slices.Clone(persisted.AllowedKeys)
	if owner.Authorize(unknownFP) == nil || owner.Authorize(clientFP) != nil || owner.Authorize(serverFP) != nil {
		t.Fatal("returned policy aliases runtime authorization")
	}
	for _, name := range []string{"remove", "revoke", "stale", "same source different", "authority"} {
		t.Run(name, func(t *testing.T) {
			bad := snapshot
			bad.SourceRevision = 3
			switch name {
			case "remove":
				bad.AllowedKeys = []string{serverFP}
			case "revoke":
				bad.AllowedKeys = []string{serverFP}
				bad.RevokedKeys = []string{clientFP}
			case "stale":
				bad.SourceRevision = 1
			case "same source different":
				bad.SourceRevision = 2
				bad.AllowedKeys = append(slices.Clone(snapshot.AllowedKeys), unknownFP)
				slices.Sort(bad.AllowedKeys)
			case "authority":
				bad.CAFingerprint = strings.Repeat("f", 64)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if result, err := owner.Apply(bad); err == nil || !reflect.DeepEqual(result, TransportPolicy{}) {
				t.Fatalf("invalid live update %#v %v", result, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatalf("rejected update changed durable policy %v", err)
			}
			if owner.Authorize(unknownFP) == nil || owner.Authorize(clientFP) != nil {
				t.Fatal("rejected update changed memory permissions")
			}
		})
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if owner.Authorize(serverFP) == nil {
		t.Fatal("closed owner authorized a handshake")
	}
	if _, serverErr := exchangeTLS(t, server, client); serverErr == nil {
		t.Fatal("old TLS callback survived owner close")
	}
	if result, err := owner.Apply(snapshot); err == nil || !reflect.DeepEqual(result, TransportPolicy{}) {
		t.Fatalf("closed owner published %#v %v", result, err)
	}
	if _, _, err := owner.TLSConfigs(fixture.trust, serverCert, serverKey, fixture.now); err == nil {
		t.Fatal("closed owner created TLS configs")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTransportPolicySnapshot(path, snapshot); err != nil {
		t.Fatalf("closed owner retained external policy lock: %v", err)
	}
}

func TestRuntimeTransportPolicySerializesApplyAuthorizeAndClose(t *testing.T) {
	path, policy := policyFixture(t)
	policy.SourceRevision = 1
	if err := PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenRuntimeTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := policy
	snapshot.Revision = 0
	snapshot.SourceRevision = 2
	snapshot.AllowedKeys = append(slices.Clone(policy.AllowedKeys), strings.Repeat("c", 64))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 10 {
				_ = owner.Authorize(policy.AllowedKeys[0])
				_, _ = owner.Apply(snapshot)
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); <-start; _ = owner.Close() }()
	close(start)
	wg.Wait()
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if owner.Authorize(policy.AllowedKeys[0]) == nil {
		t.Fatal("close race left owner authorizing")
	}
	persisted, err := LoadTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.SourceRevision != 1 && persisted.SourceRevision != 2 {
		t.Fatalf("invalid final revision %#v", persisted)
	}
	reopened, err := OpenRuntimeTransportPolicy(path)
	if err != nil {
		t.Fatal("lease not released", err)
	}
	defer reopened.Close()
	if err := reopened.Authorize(policy.AllowedKeys[0]); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyTransportPolicySnapshot(path, snapshot); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("old Close released new owner's lock: %v", err)
	}
}

func TestRuntimeTransportPolicyOpenRejectsInvalidAndReleasesLease(t *testing.T) {
	path, policy := policyFixture(t)
	if _, err := OpenRuntimeTransportPolicy(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing policy %v", err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRuntimeTransportPolicy(path); err == nil {
		t.Fatal("corrupt policy opened")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenRuntimeTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := OpenRuntimeTransportPolicy(filepath.Join(filepath.Dir(path), "transport.json")); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("second owner obtained lease: %v", err)
	}
}

func TestRuntimeTransportPolicyRejectsMissingAndRegressedDurableState(t *testing.T) {
	path, policy := policyFixture(t)
	policy.SourceRevision = 10
	policy.Revision = 1
	if err := PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenRuntimeTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	next := policy
	next.Revision = 0
	next.SourceRevision = 20
	next.AllowedKeys = append(slices.Clone(policy.AllowedKeys), strings.Repeat("c", 64))
	current, err := owner.Apply(next)
	if err != nil {
		t.Fatal(err)
	}
	pending := next
	pending.SourceRevision = 30
	pending.AllowedKeys = append(slices.Clone(next.AllowedKeys), strings.Repeat("d", 64))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "local rollback", "source rollback", "same source changed keys"} {
		t.Run(name, func(t *testing.T) {
			altered := current
			switch name {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "local rollback":
				altered.Revision--
			case "source rollback":
				altered.SourceRevision--
			case "same source changed keys":
				altered.Revision++
				altered.AllowedKeys = slices.Clone(policy.AllowedKeys)
			}
			if name != "missing" {
				data, err := json.Marshal(altered)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := owner.Apply(pending); err == nil || !reflect.DeepEqual(result, TransportPolicy{}) {
				t.Fatalf("bad durable state accepted %#v %v", result, err)
			}
			if name == "missing" {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("missing policy recreated: %v", err)
				}
			}
			if owner.Authorize(strings.Repeat("d", 64)) == nil || owner.Authorize(strings.Repeat("c", 64)) != nil {
				t.Fatal("failed publication changed live authority")
			}
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A newer valid file represents an earlier unconfirmed publication, not
	// rollback. Its exact explicit retry must confirm durability then expand RAM.
	future := pending
	future.Revision = current.Revision + 1
	data, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if owner.Authorize(strings.Repeat("d", 64)) == nil {
		t.Fatal("unconfirmed disk state changed in-memory authorization")
	}
	confirmed, err := owner.Apply(pending)
	if err != nil || !reflect.DeepEqual(confirmed, future) {
		t.Fatalf("higher durable state not confirmed %#v %v", confirmed, err)
	}
	if owner.Authorize(strings.Repeat("d", 64)) != nil {
		t.Fatal("confirmed durable state did not expand authority")
	}
}

func TestRuntimeTransportPolicySnapshotUsesActivatedMemory(t *testing.T) {
	path, policy := policyFixture(t)
	policy.SourceRevision = 10
	policy.RevokedKeys = []string{strings.Repeat("c", 64)}
	if err := PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenRuntimeTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	future := policy
	future.Revision = 2
	future.SourceRevision = 20
	future.AllowedKeys = append(slices.Clone(policy.AllowedKeys), strings.Repeat("d", 64))
	data, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := owner.Snapshot()
	if err != nil || !reflect.DeepEqual(snapshot, policy) {
		t.Fatalf("unconfirmed disk state reported as applied %#v %v", snapshot, err)
	}
	snapshot.AllowedKeys[0] = strings.Repeat("d", 64)
	snapshot.RevokedKeys[0] = strings.Repeat("e", 64)
	again, err := owner.Snapshot()
	if err != nil || !reflect.DeepEqual(again, policy) {
		t.Fatalf("snapshot aliases active state %#v %v", again, err)
	}
	pending := future
	pending.Revision = 0
	if _, err := owner.Apply(pending); err != nil {
		t.Fatal(err)
	}
	applied, err := owner.Snapshot()
	if err != nil || !reflect.DeepEqual(applied, future) {
		t.Fatalf("confirmed state not reported %#v %v", applied, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if result, err := owner.Snapshot(); err == nil || !reflect.DeepEqual(result, TransportPolicy{}) {
		t.Fatalf("closed owner returned state %#v %v", result, err)
	}
}
