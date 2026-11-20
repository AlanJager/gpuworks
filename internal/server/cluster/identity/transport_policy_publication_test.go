package identity

import (
	"crypto/tls"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestRuntimeTransportPublicationKeepsExistingTLSAvailable(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "confirmed"
		if uncertain {
			name = "unknown-confirmation"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newIdentityFixture(t)
			serverCert, serverKey, serverFP := fixture.member(t, "127.0.0.1:9010")
			oldCert, oldKey, oldFP := fixture.member(t, "127.0.0.1:9011")
			newCert, newKey, newFP := fixture.member(t, "127.0.0.1:9012")
			path, policy := policyFixture(t)
			pin, err := CertificateFingerprint(fixture.ca)
			if err != nil {
				t.Fatal(err)
			}
			policy.ClusterID = fixture.cluster
			policy.CAFingerprint = pin
			policy.SourceRevision = 1
			policy.AllowedKeys = []string{serverFP, oldFP}
			slices.Sort(policy.AllowedKeys)
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
			clientConfig := func(cert, key []byte) *tls.Config {
				_, client, err := fixture.trust.TLSConfigs(cert, key, fixture.now, func(got string) error {
					if got != serverFP {
						return errors.New("unexpected server key")
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return client
			}
			oldClient, newClient := clientConfig(oldCert, oldKey), clientConfig(newCert, newKey)
			next := policy
			next.Revision = 0
			next.SourceRevision = 2
			next.AllowedKeys = append(slices.Clone(policy.AllowedKeys), newFP)
			slices.Sort(next.AllowedKeys)
			entered, release := make(chan struct{}), make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			completed := make(chan error, 1)
			unknown := errors.New("publication confirmation unavailable")
			go func() {
				_, err := owner.apply(next, func(path string, snapshot TransportPolicy, minimum uint64) (TransportPolicy, error) {
					// Real validation, file publication and directory sync run first. The
					// caller cannot activate any new key until this result is confirmed.
					persisted, err := applyTransportPolicySnapshotLocked(path, snapshot, minimum)
					close(entered)
					<-release
					if err != nil {
						return TransportPolicy{}, err
					}
					if uncertain {
						return TransportPolicy{}, unknown
					}
					return persisted, nil
				})
				completed <- err
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("publication did not reach confirmation boundary")
			}
			disk, err := LoadTransportPolicy(path)
			if err != nil || disk.SourceRevision != 2 {
				t.Fatalf("real publication missing: %#v %v", disk, err)
			}
			oldTLS := make(chan error, 1)
			go func() {
				clientErr, serverErr := exchangeTLS(t, server, oldClient)
				oldTLS <- errors.Join(clientErr, serverErr)
			}()
			select {
			case err := <-oldTLS:
				if err != nil {
					t.Fatalf("existing authenticated TLS peer failed during publication: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("existing authenticated TLS peer blocked by publication confirmation")
			}
			if _, serverErr := exchangeTLS(t, server, newClient); serverErr == nil {
				t.Fatal("new TLS peer authorized before confirmation")
			}
			observed, err := owner.Snapshot()
			if err != nil || !reflect.DeepEqual(observed, policy) {
				t.Fatalf("unconfirmed disk state escaped through Snapshot: %#v %v", observed, err)
			}
			close(release)
			released = true
			err = <-completed
			if uncertain {
				if !errors.Is(err, unknown) {
					t.Fatalf("unknown publication result: %v", err)
				}
				if _, serverErr := exchangeTLS(t, server, newClient); serverErr == nil {
					t.Fatal("unknown confirmation authorized new TLS peer")
				}
				if clientErr, serverErr := exchangeTLS(t, server, oldClient); clientErr != nil || serverErr != nil {
					t.Fatalf("unknown confirmation lost old authority: %v %v", clientErr, serverErr)
				}
				retryCalls := 0
				_, err = owner.apply(next, func(path string, snapshot TransportPolicy, minimum uint64) (TransportPolicy, error) {
					retryCalls++
					if minimum != disk.Revision {
						return TransportPolicy{}, errors.New("retry lost durable high-water mark")
					}
					return applyTransportPolicySnapshotLocked(path, snapshot, minimum)
				})
				if err != nil || retryCalls != 1 {
					t.Fatalf("explicit retry did not reconfirm durable publication: calls=%d err=%v", retryCalls, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if clientErr, serverErr := exchangeTLS(t, server, newClient); clientErr != nil || serverErr != nil {
				t.Fatalf("confirmed new peer denied: %v %v", clientErr, serverErr)
			}
			observed, err = owner.Snapshot()
			if err != nil || observed.SourceRevision != 2 || observed.Revision != 2 {
				t.Fatalf("confirmed snapshot: %#v %v", observed, err)
			}
		})
	}
}
