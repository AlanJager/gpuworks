//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNativeCandidateSourceGrantFaultsStayClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	db := f.stores[0].DB().WithContext(ctx)
	var admission store.ClusterAdmission
	if err := db.Where("key_fingerprint = ?", f.keys[1]).First(&admission).Error; err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"negative", "exhausted", "ignored-update", "failed-update"} {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "negative":
				conn, err := f.databases[0].SQL.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if _, err := conn.ExecContext(ctx, "PRAGMA ignore_check_constraints=ON"); err != nil {
					t.Fatal(err)
				}
				_, updateErr := conn.ExecContext(ctx, "UPDATE cluster_admissions SET material_grant_sequence=-1 WHERE key_fingerprint=?", f.keys[1])
				_, resetErr := conn.ExecContext(ctx, "PRAGMA ignore_check_constraints=OFF")
				if updateErr != nil || resetErr != nil {
					t.Fatalf("install legacy invalid counter: %v / %v", updateErr, resetErr)
				}
			case "exhausted":
				if err := db.Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", f.keys[1]).UpdateColumn("material_grant_sequence", int64(math.MaxInt64)).Error; err != nil {
					t.Fatal(err)
				}
			default:
				raise := "IGNORE"
				if name == "failed-update" {
					raise = "ABORT, 'injected source grant failure'"
				}
				statement := fmt.Sprintf("CREATE TRIGGER candidate_source_grant_failure BEFORE UPDATE OF material_grant_sequence ON cluster_admissions WHEN NEW.key_fingerprint='%s' BEGIN SELECT RAISE(%s); END", f.keys[1], raise)
				if err := db.Exec(statement).Error; err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				if err := db.Exec("DROP TRIGGER IF EXISTS candidate_source_grant_failure").Error; err != nil {
					t.Error(err)
				}
				if err := db.Model(&store.ClusterAdmission{}).Where("key_fingerprint = ?", f.keys[1]).UpdateColumn("material_grant_sequence", admission.MaterialGrantSequence).Error; err != nil {
					t.Error(err)
				}
			})
			requests := f.requests.Load()
			ready, metrics, err := f.prove(ctx)
			if ready || err != nil || metrics.MembershipGrant.Calls != 1 || f.requests.Load() != requests {
				t.Fatalf("bad source grant authorized/read peers: ready=%t grants=%d remote=%d err=%v", ready, metrics.MembershipGrant.Calls, f.requests.Load()-requests, err)
			}
		})
	}
}

// Mutate actual replicated state or canonical bytes only after both first
// source checks, while authenticated batch requests are in flight. The second
// local checks and the source/final grants must close the proof.
func TestNativeCandidateProofRejectsChangesDuringRemoteReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	db := f.stores[0].DB().WithContext(ctx)
	var admission store.ClusterAdmission
	if err := db.Where("key_fingerprint = ?", f.keys[1]).First(&admission).Error; err != nil {
		t.Fatal(err)
	}
	asset := []byte("tiny agent override")
	digest := materialTestDigest(asset)
	path := filepath.Join(f.receivers[0].root, digest[:2], digest)
	for _, name := range []string{"revoked-source", "replaced-key", "changed-origin", "changed-source-id", "changed-pointer-time", "corrupt-bytes", "redirected-index", "failed-final-source-grant"} {
		t.Run(name, func(t *testing.T) {
			var once sync.Once
			result := make(chan error, 1)
			f.mu.Lock()
			f.before = func() {
				once.Do(func() {
					var err error
					switch name {
					case "revoked-source":
						_, err = f.stores[0].RevokeClusterAdmission(ctx, f.expected().ClusterID, f.keys[1])
					case "replaced-key":
						err = db.Model(&store.ClusterAdmission{}).Where("address = ?", admission.Address).UpdateColumn("key_fingerprint", strings.Repeat("f", 64)).Error
					case "changed-origin":
						err = f.stores[0].SetClusterEnrollmentOriginForVerifiedKey(ctx, f.expected(), f.keys[1], "https://127.0.0.1:1")
					case "changed-source-id":
						err = db.Model(&store.MaterialManifestRecord{}).Where("source_member_id = ?", f.pointers[0].SourceMemberID).UpdateColumn("source_member_id", int64(99999)).Error
					case "changed-pointer-time":
						err = db.Model(&store.MaterialManifestRecord{}).Where("source_member_id = ?", f.pointers[0].SourceMemberID).UpdateColumn("recorded_at", time.Now().UTC().Add(time.Hour)).Error
					case "corrupt-bytes":
						err = os.WriteFile(path, []byte("tiny agent overridX"), 0600)
					case "redirected-index":
						err = db.Model(&store.MaterialReplica{}).Where("digest = ? AND member_id = ?", digest, f.receivers[0].memberID).UpdateColumn("path", path+".redirected").Error
					case "failed-final-source-grant":
						err = db.Exec(fmt.Sprintf("CREATE TRIGGER candidate_source_grant_failure BEFORE UPDATE OF material_grant_sequence ON cluster_admissions WHEN NEW.key_fingerprint='%s' BEGIN SELECT RAISE(ABORT, 'injected final source grant failure'); END", f.keys[1])).Error
					}
					result <- err
				})
			}
			f.mu.Unlock()
			t.Cleanup(func() {
				f.mu.Lock()
				f.before = nil
				f.mu.Unlock()
				for _, err := range []error{
					db.Exec("DROP TRIGGER IF EXISTS candidate_source_grant_failure").Error,
					db.Model(&store.ClusterAdmission{}).Where("address = ?", admission.Address).Updates(map[string]any{"key_fingerprint": admission.KeyFingerprint, "revoked_at": nil, "enrollment_origin": admission.EnrollmentOrigin}).Error,
					db.Model(&store.MaterialManifestRecord{}).Where("source_key = ?", f.keys[1]).Updates(map[string]any{"source_member_id": f.pointers[0].SourceMemberID, "recorded_at": f.pointers[0].RecordedAt}).Error,
					os.WriteFile(path, asset, 0600),
					db.Model(&store.MaterialReplica{}).Where("digest = ? AND member_id = ?", digest, f.receivers[0].memberID).UpdateColumn("path", path).Error,
				} {
					if err != nil {
						t.Error(err)
					}
				}
			})
			ready, metrics, err := f.prove(ctx)
			select {
			case faultErr := <-result:
				if faultErr != nil {
					t.Fatal(faultErr)
				}
			default:
				t.Fatal("did not reach authenticated remote-read seam")
			}
			if ready || err != nil || metrics.MembershipGrant.Calls < 1 || metrics.MembershipGrant.Calls > 2 {
				t.Fatalf("changed proof accepted: ready=%t grants=%d err=%v", ready, metrics.MembershipGrant.Calls, err)
			}
		})
	}
	ready, _, err := f.prove(ctx)
	if !ready || err != nil {
		t.Fatalf("restored exact bytes/authority did not recover: %t %v", ready, err)
	}
}

func (f *candidateGrantFixture) expected() store.ClusterIdentity {
	info := f.bundles[0].info
	return store.ClusterIdentity{ClusterID: info.ClusterID, CAFingerprint: info.CAFingerprint, Protocol: info.Protocol}
}

func TestNativeCandidateProofRequiresCurrentQuorum(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	for _, db := range f.databases[1:] {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	bounded, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	ready, _, err := f.prove(bounded)
	if ready || err == nil {
		t.Fatalf("isolated native voter authorized candidate: %t %v", ready, err)
	}
	if f.requests.Load() != 0 {
		t.Fatal("read peers without a fresh quorum grant")
	}
}

func TestNativeCandidateProofRejectsLiveSourceIDRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	sourceID := f.databases[1].ID
	var once sync.Once
	result := make(chan error, 1)
	f.before = func() { once.Do(func() { result <- f.databases[0].Remove(ctx, sourceID) }) }
	ready, metrics, err := f.prove(ctx)
	select {
	case removeErr := <-result:
		if removeErr != nil {
			t.Fatal(removeErr)
		}
	default:
		t.Fatal("did not reach real remote-read membership transition")
	}
	// Depending on where the removal lands, the proof either declines
	// readiness or reports the shrink as an interrupted proof
	// (DeadlineExceeded). It never reports readiness or another failure.
	if ready || (err != nil && !errors.Is(err, context.DeadlineExceeded)) || metrics.MembershipGrant.Calls < 1 || metrics.MembershipGrant.Calls > 2 {
		t.Fatalf("changed live source ID accepted: ready=%t grants=%d err=%v", ready, metrics.MembershipGrant.Calls, err)
	}
	members, err := f.databases[0].Members(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.ID == sourceID {
			t.Fatal("fixture did not actually remove live source ID")
		}
	}
}
