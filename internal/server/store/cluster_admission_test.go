package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func admissionFixture(t *testing.T) (*Store, ClusterIdentity, uint) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "identity.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	st.db = st.db.Session(&gorm.Session{NowFunc: func() time.Time { return now }})
	identity, err := st.InitializeClusterIdentity(t.Context(), ClusterIdentity{ClusterID: strings.Repeat("a", 32), CAFingerprint: strings.Repeat("b", 64), Protocol: ClusterProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	administrator := User{Username: "cluster-admin", Role: "admin"}
	if err := st.db.Create(&administrator).Error; err != nil {
		t.Fatal(err)
	}
	return st, identity, administrator.ID
}

func TestClusterIdentityExplicitAndImmutable(t *testing.T) {
	st, identity, _ := admissionFixture(t)
	repeated, err := st.InitializeClusterIdentity(t.Context(), identity)
	if err != nil || repeated != identity {
		t.Fatalf("retry: %+v %v", repeated, err)
	}
	changed := identity
	changed.CAFingerprint = strings.Repeat("c", 64)
	if _, err := st.InitializeClusterIdentity(t.Context(), changed); !errors.Is(err, ErrClusterIdentityConflict) {
		t.Fatalf("changed CA: %v", err)
	}
	changed = identity
	changed.ClusterID = strings.Repeat("d", 32)
	if _, err := st.InitializeClusterIdentity(t.Context(), changed); !errors.Is(err, ErrClusterIdentityConflict) {
		t.Fatalf("changed cluster: %v", err)
	}
	if err := st.db.Create(&ClusterIdentity{ID: 2, ClusterID: strings.Repeat("e", 32), Protocol: 1, CAFingerprint: strings.Repeat("f", 64)}).Error; err == nil {
		t.Fatal("singleton DB constraint missing")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClusterIdentity(t.Context()); err == nil {
		t.Fatal("closed database read succeeded")
	}
	if _, err := st.InitializeClusterIdentity(t.Context(), identity); err == nil {
		t.Fatal("database read failure generated replacement identity")
	}
}

func TestClusterInvitationRejectsUnauthorizedAndExpired(t *testing.T) {
	st, identity, administrator := admissionFixture(t)
	for _, issuer := range []uint{0, administrator + 1} {
		if _, token, err := st.IssueClusterInvitation(t.Context(), identity.ClusterID, issuer, time.Minute); !errors.Is(err, ErrClusterAdministratorRequired) || token != "" {
			t.Fatalf("issuer=%d token returned=%t err=%v", issuer, token != "", err)
		}
	}
	for _, ttl := range []time.Duration{0, -time.Second, MaxClusterInvitationTTL + time.Second} {
		if _, _, err := st.IssueClusterInvitation(t.Context(), identity.ClusterID, administrator, ttl); err == nil {
			t.Fatal("invalid expiry accepted")
		}
	}
	invitation, token, err := st.IssueClusterInvitation(t.Context(), identity.ClusterID, administrator, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 || invitation.TokenHash == token || invitation.TokenHash != HashToken(token) {
		t.Fatal("bearer token stored directly")
	}
	var persisted ClusterInvitation
	if err := st.db.First(&persisted, "token_hash = ?", HashToken(token)).Error; err != nil {
		t.Fatal(err)
	}
	if persisted != invitation {
		t.Fatal("invitation changed")
	}
	key := strings.Repeat("c", 64)
	if _, err := st.ClaimClusterInvitation(t.Context(), strings.Repeat("d", 32), token, key, "127.0.0.1:9010"); !errors.Is(err, ErrClusterIdentityConflict) {
		t.Fatal(err)
	}
	if _, err := st.ClaimClusterInvitation(t.Context(), identity.ClusterID, strings.Repeat("f", 64), key, "127.0.0.1:9010"); !errors.Is(err, ErrClusterInvitationRejected) {
		t.Fatal(err)
	}
	// Exactly the expiry boundary rejects, without wall-clock sleeps.
	st.db = st.db.Session(&gorm.Session{NowFunc: func() time.Time { return invitation.ExpiresAt }})
	if _, err := st.ClaimClusterInvitation(t.Context(), identity.ClusterID, token, key, "127.0.0.1:9010"); !errors.Is(err, ErrClusterInvitationRejected) {
		t.Fatalf("expired: %v", err)
	}
	var count int64
	st.db.Model(&ClusterAdmission{}).Count(&count)
	if count != 0 {
		t.Fatal("invalid invitation created admission")
	}
}

func TestClusterInvitationConcurrentClaimAndResume(t *testing.T) {
	st, identity, administrator := admissionFixture(t)
	_, token, err := st.IssueClusterInvitation(t.Context(), identity.ClusterID, administrator, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan ClusterAdmission, contenders)
	for index := range contenders {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			admitted, err := st.ClaimClusterInvitation(context.Background(), identity.ClusterID, token, fmt.Sprintf("%064x", index+1), fmt.Sprintf("127.0.0.1:%d", 9100+index))
			if err == nil {
				results <- admitted
			}
		}(index)
	}
	close(start)
	wg.Wait()
	close(results)
	var successes []ClusterAdmission
	for result := range results {
		successes = append(successes, result)
	}
	if len(successes) != 1 {
		t.Fatalf("successful claim count=%d", len(successes))
	}
	winner := successes[0]
	if _, err := st.ClaimClusterInvitation(t.Context(), identity.ClusterID, token, winner.KeyFingerprint, winner.Address); !errors.Is(err, ErrClusterInvitationRejected) {
		t.Fatalf("bearer replay: %v", err)
	}
	resumed, err := st.ClusterAdmissionForVerifiedKey(t.Context(), identity.ClusterID, winner.KeyFingerprint)
	if err != nil || resumed != winner {
		t.Fatalf("resume=%+v err=%v", resumed, err)
	}
	if _, err := st.ClusterAdmissionForVerifiedKey(t.Context(), identity.ClusterID, strings.Repeat("e", 64)); !errors.Is(err, ErrClusterAdmissionRejected) {
		t.Fatalf("other machine resumed: %v", err)
	}
	var count int64
	st.db.Model(&ClusterAdmission{}).Count(&count)
	if count != 1 {
		t.Fatalf("duplicate admissions=%d", count)
	}
}

func TestClusterAdmissionConflictRollsBackInvitationConsumption(t *testing.T) {
	st, identity, administrator := admissionFixture(t)
	_, firstToken, err := st.IssueClusterInvitation(t.Context(), identity.ClusterID, administrator, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("c", 64)
	if _, err := st.ClaimClusterInvitation(t.Context(), identity.ClusterID, firstToken, key, "HOST.example:09010"); err != nil {
		t.Fatal(err)
	}
	second, token, err := st.IssueClusterInvitation(t.Context(), identity.ClusterID, administrator, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimClusterInvitation(t.Context(), identity.ClusterID, token, strings.Repeat("d", 64), "host.example.:9010"); !errors.Is(err, ErrClusterAdmissionRejected) {
		t.Fatalf("address conflict: %v", err)
	}
	if err := st.db.First(&second, "token_hash = ?", second.TokenHash).Error; err != nil {
		t.Fatal(err)
	}
	if second.ConsumedAt != nil {
		t.Fatal("failed admission consumed invitation")
	}
	if _, err := st.ClaimClusterInvitation(t.Context(), identity.ClusterID, token, strings.Repeat("d", 64), "host2.example:9010"); err != nil {
		t.Fatalf("rolled-back invitation cannot be used: %v", err)
	}
}

func TestClusterAdmissionRevokedKeyCannotResumeOrRejoin(t *testing.T) {
	st, identity, administrator := admissionFixture(t)
	_, token, err := st.IssueClusterInvitation(t.Context(), identity.ClusterID, administrator, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("c", 64)
	if _, err := st.ClaimClusterInvitation(t.Context(), identity.ClusterID, token, key, "127.0.0.1:9010"); err != nil {
		t.Fatal(err)
	}
	if err := st.db.Model(&ClusterAdmission{}).Where("key_fingerprint = ?", key).Update("revoked_at", st.db.NowFunc()).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClusterAdmissionForVerifiedKey(t.Context(), identity.ClusterID, key); !errors.Is(err, ErrClusterAdmissionRejected) {
		t.Fatalf("revoked resume: %v", err)
	}
	_, token, err = st.IssueClusterInvitation(t.Context(), identity.ClusterID, administrator, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimClusterInvitation(t.Context(), identity.ClusterID, token, key, "127.0.0.1:9010"); !errors.Is(err, ErrClusterAdmissionRejected) {
		t.Fatalf("revoked key rebound: %v", err)
	}
	if _, err := st.ClaimClusterInvitation(t.Context(), identity.ClusterID, token, strings.Repeat("d", 64), "127.0.0.1:9010"); err != nil {
		t.Fatalf("replacement address reuse: %v", err)
	}
}

func TestClusterAdmissionMigrationAtomicAndIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migration.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	defer pool.Close()
	verifyClusterAdmissionMigration(t, db)
}

func verifyClusterAdmissionMigration(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	// A conflicting real view makes creation of the second resource fail.
	if err := db.Exec("CREATE VIEW cluster_invitations AS SELECT 1 AS token_hash").Error; err != nil {
		t.Fatal(err)
	}
	var change migration
	for _, candidate := range migrations {
		if candidate.Name == "cluster_admission" {
			change = candidate
			break
		}
	}
	if change.Apply == nil {
		t.Fatal("cluster admission migration missing")
	}
	if err := applyMigration(db, change); err == nil {
		t.Fatal("conflicting schema accepted")
	}
	if db.Migrator().HasTable(&ClusterIdentity{}) {
		t.Fatal("partial schema committed")
	}
	var count int64
	db.Model(&SchemaMigration{}).Where("version = ?", change.Version).Count(&count)
	if count != 0 {
		t.Fatal("failed migration marker committed")
	}
	if err := db.Exec("DROP VIEW cluster_invitations").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateDB(db, []migration{change}); err != nil {
			t.Fatal(err)
		}
	}
	for _, entity := range []any{&ClusterIdentity{}, &ClusterInvitation{}, &ClusterAdmission{}} {
		if !db.Migrator().HasTable(entity) {
			t.Fatalf("missing %T", entity)
		}
	}
	if !db.Migrator().HasIndex(&ClusterAdmission{}, "idx_cluster_admission_address") {
		t.Fatal("missing address uniqueness")
	}
	db.Model(&SchemaMigration{}).Where("version = ?", change.Version).Count(&count)
	if count != 1 {
		t.Fatalf("markers=%d", count)
	}
}

func TestClusterIdentityReadDoesNotInitialize(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "uninitialized.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.ClusterIdentity(t.Context()); !errors.Is(err, ErrClusterNotInitialized) {
		t.Fatalf("uninitialized read=%v", err)
	}
	if _, _, err := st.IssueClusterInvitation(t.Context(), strings.Repeat("a", 32), 1, time.Minute); !errors.Is(err, ErrClusterNotInitialized) {
		t.Fatalf("uninitialized invitation=%v", err)
	}
	var count int64
	if err := st.db.Model(&ClusterIdentity{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("read generated identity: count=%d err=%v", count, err)
	}
}

func TestClusterInvitationCancellationLeavesNoConsumption(t *testing.T) {
	st, identity, administrator := admissionFixture(t)
	invitation, token, err := st.IssueClusterInvitation(t.Context(), identity.ClusterID, administrator, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := st.ClaimClusterInvitation(ctx, identity.ClusterID, token, strings.Repeat("c", 64), "127.0.0.1:9010"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled claim: %v", err)
	}
	if err := st.db.First(&invitation, "token_hash = ?", invitation.TokenHash).Error; err != nil {
		t.Fatal(err)
	}
	if invitation.ConsumedAt != nil {
		t.Fatal("canceled operation consumed invitation")
	}
}
