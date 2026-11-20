//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMaterialRestartInstalledProofRestoresOnlyScheduling(t *testing.T) {
	f := newRestartMaterialFixture(t, "")
	assertRestartFixtureCoverage(t, f)
	before := restartFixtureSequence(t, f)
	gate := newMaterialEligibility(time.Now, func(context.Context) (bool, error) {
		t.Fatal("restart must not run a readiness proof")
		return false, nil
	}, materialReproofInterval)
	pointer, err := f.check(t.Context())
	if err != nil || !sameBootstrapMaterialPointers([]store.MaterialManifestRecord{pointer}, []store.MaterialManifestRecord{f.pointer}) {
		t.Fatalf("current installed proof: %+v %v", pointer, err)
	}
	if f.requests.Load() != 2 || f.transfers.Load() != 0 || f.grantCalls != 2 {
		t.Fatalf("installed%d transfers%d freshgrants%d", f.requests.Load(), f.transfers.Load(), f.grantCalls)
	}
	if delta := restartFixtureSequence(t, f) - before; delta != 4 {
		t.Fatalf("offline real committed grants=%d want4 (two local+two private)", delta)
	}
	now := time.Now()
	full, probe := materialReplicationDue(now, time.Minute, now, now, true)
	if full || probe || materialInstallationDue(now, now, time.Minute, true) {
		t.Fatal("restored observations unnecessarily scheduled first transfer")
	}
	if gate.Eligible() || !gate.deadline.IsZero() {
		t.Fatal("planning observation opened business eligibility")
	}
	var after store.MaterialManifestRecord
	if err := f.st.DB().Where("source_member_id=?", 11).First(&after).Error; err != nil || after != f.pointer {
		t.Fatal("read-only optimization republished its pointer")
	}
}

func TestMaterialRestartIncompleteOrChangedStateRequiresOriginalFullRun(t *testing.T) {
	for _, fault := range []string{"missing-own-pointer", "new-local-material", "missing-consumer", "corrupt-canonical", "missing-index", "redirect-index", "peer-missing", "peer-consumer", "peer-index", "peer-pointer", "peer-auth", "peer-receipt", "peer-tls", "phase", "origin-aba", "admission-aba", "phase-aba", "pointer-aba", "source-during-http", "canonical-during-http", "index-during-http", "new-file-during-http", "member-change"} {
		t.Run(fault, func(t *testing.T) {
			f := newRestartMaterialFixture(t, fault)
			write := func(path string, body []byte) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			canonical := func(receiver *materialReceiver, name string) string {
				body, err := os.ReadFile(filepath.Join(f.root, name))
				if err != nil {
					t.Fatal(err)
				}
				digest := materialTestDigest(body)
				return filepath.Join(receiver.root, digest[:2], digest)
			}
			own, err := newMaterialReceiver(filepath.Join(f.root, "material-replicas"), 11)
			if err != nil {
				t.Fatal(err)
			}
			agent := canonical(own, "dl/tiny-agent")
			remove := func(path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			deleteIndex := func(member uint64) {
				t.Helper()
				if err := f.st.DB().Where("member_id=? AND digest=?", int64(member), filepath.Base(agent)).Delete(&store.MaterialReplica{}).Error; err != nil {
					t.Fatal(err)
				}
			}
			switch fault {
			case "missing-own-pointer":
				if err := f.st.DB().Delete(&f.pointer).Error; err != nil {
					t.Fatal(err)
				}
			case "new-local-material":
				write(filepath.Join(f.root, "dl/new-agent"), []byte("new managed bytes"))
			case "missing-consumer":
				remove(filepath.Join(f.root, "dl/tiny-agent"))
			case "corrupt-canonical":
				write(agent, []byte("corrupt agent bytes"))
			case "missing-index":
				deleteIndex(11)
			case "redirect-index":
				if err := f.st.DB().Model(&store.MaterialReplica{}).Where("member_id=? AND digest=?", 11, filepath.Base(agent)).Update("path", filepath.Join(f.root, "dl/tiny-agent")).Error; err != nil {
					t.Fatal(err)
				}
			case "peer-missing":
				remove(canonical(f.receivers[0], "dl/tiny-agent"))
			case "peer-consumer":
				remove(filepath.Join(filepath.Dir(f.receivers[0].root), "dl/tiny-agent"))
			case "peer-index":
				deleteIndex(f.receivers[0].memberID)
			case "phase":
				state, err := f.st.ReadMaintenancePhase(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.st.TransitionMaintenancePhase(t.Context(), state, store.MaintenanceFreezing); err != nil {
					t.Fatal(err)
				}
			}
			afterHTTP := func() {
				switch fault {
				case "origin-aba":
					original := f.members[1].EnrollmentOrigin
					for _, origin := range []string{"https://127.0.0.2:9999", original} {
						if err := f.st.SetClusterEnrollmentOriginForVerifiedKey(t.Context(), f.expected, f.members[1].KeyFingerprint, origin); err != nil {
							t.Error(err)
						}
					}
				case "admission-aba":
					var admin store.User
					if err := f.st.DB().Where("username=?", "restart-fixture-admin").First(&admin).Error; err != nil {
						t.Error(err)
						return
					}
					_, token, err := f.st.IssueClusterInvitation(t.Context(), f.expected.ClusterID, admin.ID, time.Minute)
					if err != nil {
						t.Error(err)
						return
					}
					key := strings.Repeat("f", 64)
					if _, err := f.st.ClaimClusterInvitation(t.Context(), f.expected.ClusterID, token, key, "127.0.0.9:9001"); err != nil {
						t.Error(err)
						return
					}
					if _, err := f.st.RevokeClusterAdmission(t.Context(), f.expected.ClusterID, key); err != nil {
						t.Error(err)
					}
				case "phase-aba":
					state, err := f.st.ReadMaintenancePhase(t.Context())
					if err != nil {
						t.Error(err)
						return
					}
					for _, phase := range []string{store.MaintenanceFreezing, store.MaintenanceServing} {
						state, err = f.st.TransitionMaintenancePhase(t.Context(), state, phase)
						if err != nil {
							t.Error(err)
							return
						}
					}
				case "pointer-aba":
					// Offline SQL establishes the same public publication observation:
					// restored bytes retain a newer timestamp; native tests use publication.
					changed := f.pointer
					changed.Digest = strings.Repeat("b", 64)
					changed.RecordedAt = changed.RecordedAt.Add(time.Millisecond)
					if err := f.st.DB().Save(&changed).Error; err != nil {
						t.Error(err)
					}
					changed.Digest = f.pointer.Digest
					changed.RecordedAt = changed.RecordedAt.Add(time.Millisecond)
					if err := f.st.DB().Save(&changed).Error; err != nil {
						t.Error(err)
					}
				case "source-during-http":
					write(filepath.Join(f.root, enrollmentDirectory, "server.pem"), []byte("changed identity bytes"))
				case "canonical-during-http":
					write(agent, []byte("corrupt agent bytes"))
				case "index-during-http":
					deleteIndex(11)
				case "new-file-during-http":
					write(filepath.Join(f.root, "dl/new-agent"), []byte("new managed bytes"))
				case "member-change":
					f.members[1].Role = "standby"
				}
			}
			f.afterHTTP = sync.OnceFunc(afterHTTP)
			pointer, err := f.check(t.Context())
			if err == nil || pointer != (store.MaterialManifestRecord{}) {
				t.Fatalf("unsafe restore accepted: %+v %v", pointer, err)
			}
			full, _ := materialReplicationDue(time.Now(), time.Minute, time.Time{}, time.Time{}, false)
			if !full {
				t.Fatal("failed optimization lost original same-cycle full replication")
			}
			if f.transfers.Load() != 0 {
				t.Fatal("optimization itself transferred or materialized bytes")
			}
			if fault == "source-during-http" || fault == "canonical-during-http" || fault == "index-during-http" || fault == "new-file-during-http" {
				if f.requests.Load() != 2 {
					t.Fatal("post-HTTP mutation was rejected without completing both actual peer checks")
				}
			}
		})
	}
}

func TestMaterialRestartCancellationDoesNotFallBackToTransfer(t *testing.T) {
	t.Run("already-canceled", func(t *testing.T) {
		f := newRestartMaterialFixture(t, "")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		pointer, err := f.check(ctx)
		if !errors.Is(err, context.Canceled) || pointer != (store.MaterialManifestRecord{}) || f.grantCalls != 0 || f.requests.Load() != 0 {
			t.Fatalf("canceled restart consumed work: pointer%+v err%v grants%d requests%d", pointer, err, f.grantCalls, f.requests.Load())
		}
	})
	t.Run("during-http", func(t *testing.T) {
		f := newRestartMaterialFixture(t, "http-cancel")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f.afterHTTP = cancel
		pointer, err := f.check(ctx)
		if !errors.Is(err, context.Canceled) || pointer != (store.MaterialManifestRecord{}) || f.grantCalls != 1 || f.requests.Load() != 1 || f.transfers.Load() != 0 {
			t.Fatalf("in-flight cancellation: %+v %v grants%d requests%d", pointer, err, f.grantCalls, f.requests.Load())
		}
	})
}
