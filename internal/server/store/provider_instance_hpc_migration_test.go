package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// A database whose cloud instances were synced before HPC clusters gains the
// columns in one transaction; the old rows keep what they had and read as
// instances of no HPC cluster, and a sync records the cluster.
func TestProviderInstanceHPCClusterMigration(t *testing.T) {
	s := newTestStore(t)
	p := &Provider{Name: "ali", Kind: "aliyun"}
	if err := s.db.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&ProviderInstance{ProviderID: p.ID, ExternalID: "i-1", Name: "ehpc-sz-1_gpu000", GPUCount: 1, Kind: "vm", NodeID: 9}).Error; err != nil {
		t.Fatal(err)
	}
	cols := []string{"hpc_cluster_id", "hpc_cluster_name", "hpc_scheduler", "hpc_scheduler_version"}
	if err := dropColumns(s.db, "provider_instances", cols...); err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "provider_instance_hpc_cluster")
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}

	boom := errors.New("after columns")
	failed := m
	failed.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}
	if err := applyMigration(s.db, failed); !errors.Is(err, boom) {
		t.Fatalf("rollback = %v", err)
	}
	for _, col := range providerInstanceHPCColumns {
		if s.db.Migrator().HasColumn(&ProviderInstance{}, col) {
			t.Fatalf("the failed transaction kept %s", col)
		}
	}
	var count int64
	if err := s.db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("the failed transaction kept the marker: %d %v", count, err)
	}

	for range 2 {
		if err := migrateDB(s.db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	for _, col := range providerInstanceHPCColumns {
		if !s.db.Migrator().HasColumn(&ProviderInstance{}, col) {
			t.Fatalf("provider_instances lacks %s", col)
		}
	}
	var marker SchemaMigration
	if err := s.db.First(&marker, m.Version).Error; err != nil || marker.Name != "provider_instance_hpc_cluster" {
		t.Fatalf("marker = %+v %v", marker, err)
	}
	row, err := s.ProviderInstanceByExternalID(p.ID, "i-1")
	if err != nil || row == nil || row.HPCClusterID != "" || row.NodeID != 9 || row.Name != "ehpc-sz-1_gpu000" {
		t.Fatalf("legacy row = %+v (%v)", row, err)
	}

	// The next sync records the cluster and keeps the node link.
	if err := s.SyncProviderInstances(p.ID, []ProviderInstance{{ExternalID: "i-1", Name: "ehpc-sz-1_gpu000", GPUCount: 1, Kind: "vm",
		HPCClusterID: "ehpc-sz-1", HPCClusterName: "demo", HPCScheduler: "slurm", HPCSchedulerVersion: "22.05.8"}}); err != nil {
		t.Fatal(err)
	}
	row, _ = s.ProviderInstanceByExternalID(p.ID, "i-1")
	if row.HPCClusterID != "ehpc-sz-1" || row.HPCClusterName != "demo" || row.HPCScheduler != "slurm" || row.HPCSchedulerVersion != "22.05.8" || row.NodeID != 9 {
		t.Fatalf("synced row = %+v", row)
	}
	// A sync that no longer finds it in a cluster clears it.
	if err := s.SyncProviderInstances(p.ID, []ProviderInstance{{ExternalID: "i-1", Name: "renamed", GPUCount: 1, Kind: "vm"}}); err != nil {
		t.Fatal(err)
	}
	row, _ = s.ProviderInstanceByExternalID(p.ID, "i-1")
	if row.HPCClusterID != "" || row.HPCScheduler != "" {
		t.Fatalf("a node out of its cluster = %+v", row)
	}

	// After a rollback the next start applies it.
	if err := dropColumns(s.db, "provider_instances", cols...); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(s.db, []migration{m}); err != nil || !s.db.Migrator().HasColumn(&ProviderInstance{}, "HPCScheduler") {
		t.Fatalf("restart after rollback: %v", err)
	}
}
