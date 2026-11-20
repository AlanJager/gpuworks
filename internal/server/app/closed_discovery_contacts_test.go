//go:build linux && dqlite

package app

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// This exercises the production listener and public caller with independently
// supplied contacts. The fixture's fixed reader is not native quorum evidence,
// and the alternate origin is not discovered from opaque replica files.
func TestClosedDiscoveryContactsOriginalListenerDown(t *testing.T) {
	for _, mode := range []string{"alternate", "no-alternate", "unallowed-key", "revoked-requester"} {
		t.Run(mode, func(t *testing.T) {
			f, session := newClosedDiscoveryRouteFixture(t)
			original := startClosedDiscoveryFixture(t, f, session)
			alternate := startClosedDiscoveryFixture(t, f, session)
			originalOrigin := "https://" + original.Addr()
			alternateOrigin := "https://" + alternate.Addr()
			if originalOrigin == alternateOrigin {
				t.Fatal("listeners did not have independent sockets")
			}
			closeCtx, stopClose := context.WithTimeout(t.Context(), 3*time.Second)
			if err := original.Close(closeCtx); err != nil {
				stopClose()
				t.Fatal(err)
			}
			stopClose()
			// The closed original tracker cannot admit any later reader call.
			for i := range session.members {
				if session.members[i].KeyFingerprint == f.serverKey {
					session.members[i].EnrollmentOrigin = alternateOrigin
				}
			}
			for i := range session.admissions {
				if session.admissions[i].KeyFingerprint == f.serverKey {
					session.admissions[i].EnrollmentOrigin = alternateOrigin
				}
			}
			contacts := map[string]string{alternateOrigin: f.serverKey}
			switch mode {
			case "no-alternate":
				contacts = nil
			case "unallowed-key":
				contacts[alternateOrigin] = strings.Repeat("f", 64)
			case "revoked-requester":
				now := time.Now().UTC()
				session.admissions[1].RevokedAt = &now
			}
			before := append([]store.ClusterAdmission(nil), session.admissions...)
			beforeMembers := append([]store.ClusterTransportMember(nil), session.members...)
			caller, err := enrollment.NewClient(originalOrigin+"/api/v1/cluster/enrollment/certificate", f.bundle.info.ClusterID, f.bundle.info.CAFingerprint, f.bundle.info.Protocol)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(caller.Close)
			certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.requesterPair.Certificate[0]})
			private, err := x509.MarshalPKCS8PrivateKey(f.requesterPair.PrivateKey)
			if err != nil {
				t.Fatal(err)
			}
			key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
			allowed := []string{f.serverKey, f.requesterKey, f.otherKey}
			sort.Strings(allowed)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			facts, err := caller.DiscoverClusterQuorumFactsFromContacts(ctx, certificate, key, allowed, contacts)
			if mode == "alternate" {
				if err != nil || facts.ClusterID != f.bundle.info.ClusterID || facts.CAFingerprint != f.bundle.info.CAFingerprint || facts.LeaderID != 7 || facts.Term != 4 || facts.Index != 42 || facts.PhaseRevision != 3 || facts.Nonce == "" {
					t.Fatalf("fresh public facts=%+v err=%v", facts, err)
				}
				if len(facts.Members) != 3 || facts.Members[0].EnrollmentOrigin != alternateOrigin || facts.Members[0].KeyFingerprint != f.serverKey {
					t.Fatalf("alternate actual provider differs: %+v", facts.Members)
				}
			} else if err == nil || !reflect.DeepEqual(facts, enrollment.ClusterQuorumFacts{}) {
				t.Fatalf("rejected discovery published facts=%+v err=%v", facts, err)
			}
			session.mu.Lock()
			calls, discoveries := session.calls, session.discoveryCalls
			unchanged := reflect.DeepEqual(before, session.admissions) && reflect.DeepEqual(beforeMembers, session.members)
			session.mu.Unlock()
			wantFacts, wantDiscovery := 0, 0
			if mode == "alternate" {
				wantFacts, wantDiscovery = 1, 1
			} else if mode == "revoked-requester" {
				wantDiscovery = 1
			}
			if calls != wantFacts || discoveries != wantDiscovery || !unchanged {
				t.Fatalf("facts=%d discovery=%d unchanged=%v", calls, discoveries, unchanged)
			}
		})
	}
}
