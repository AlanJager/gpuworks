package main

import (
	"os"
	"path/filepath"
	"testing"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
)

func TestFakeNetworkInventory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "net.json")
	inventory := `{"interfaces":[{"name":"bond1","state":"up","addrs":["10.113.46.22/24"]}],
"rdma_ports":[{"device":"mlx5_2","port":1,"state":"ACTIVE","link_layer":"InfiniBand"}],
"management":{"interface":"bond1","ip":"10.113.46.22"}}`
	if err := os.WriteFile(path, []byte(inventory), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg agentapp.Config
	if err := applyFakeNetwork(&cfg, path); err != nil {
		t.Fatal(err)
	}
	inv := cfg.NetworkInventory()
	if inv.Management.IP != "10.113.46.22" || len(inv.RDMAPorts) != 1 || inv.RDMAPorts[0].Device != "mlx5_2" {
		t.Fatalf("inventory = %+v", inv)
	}
	if name, err := cfg.NetworkInterfaceResolver("10.113.46.22"); err != nil || name != "bond1" {
		t.Fatalf("resolve = %q %v", name, err)
	}
	if _, err := cfg.NetworkInterfaceResolver("10.0.0.9"); err == nil {
		t.Fatal("an address the inventory does not hold resolved")
	}
	for name, body := range map[string]string{"missing management": `{"interfaces":[]}`, "not json": `{`} {
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := applyFakeNetwork(&agentapp.Config{}, bad); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if err := applyFakeNetwork(&agentapp.Config{}, filepath.Join(dir, "absent.json")); err == nil {
		t.Fatal("a missing file was accepted")
	}
}
