package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestProviderSSHConnectionsMigration(t *testing.T) {
	st, err := Open("file:provider_ssh_connections_migration?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.CurrentMigration()
	if err != nil {
		t.Fatal(err)
	}
	if version < 45 {
		t.Fatalf("current migration = %d, want at least 45", version)
	}
	if !st.db.Migrator().HasColumn(&ProviderInstance{}, "SSHConnections") {
		t.Fatal("provider_instances.ssh_connections column is missing")
	}
}

func TestUpsertProviderInstanceClearsRemovedSSHConnections(t *testing.T) {
	st, err := Open("file:provider_ssh_connections_clear?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{Name: "provider", Kind: "test"}
	if err := st.db.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProviderInstance(&ProviderInstance{ProviderID: p.ID, ExternalID: "i-1", SSHConnections: `[{"id":"direct"}]`, SSHPort: 22}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProviderInstance(&ProviderInstance{ProviderID: p.ID, ExternalID: "i-1", SSHConnections: "[]"}); err != nil {
		t.Fatal(err)
	}
	instances, err := st.ProviderInstances(p.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances = %+v, err = %v", instances, err)
	}
	if instances[0].SSHConnections != "[]" || instances[0].SSHPort != 0 {
		t.Fatalf("stale SSH metadata = %+v", instances[0])
	}
}
