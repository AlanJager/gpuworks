//go:build linux

package extplugin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
)

// A real server answers an installed plugin's node reads: the plugin pages
// the inventory and reads the identity of the nodes on it, and the host
// digest is keyed with the salt bound to the package. The same package
// without the permissions is refused and learns why.
func TestContractPluginReadsNodes(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	st := sv.a.Store()
	first := node(t, st, "gpu-a01", 2)
	node(t, st, "gpu-a02", 1)
	const machine = "4c4c4544-0042-3510-8051-b4c04f4e3432"
	if _, err := st.SetNodeHostSystemUUID(first.UUID, machine); err != nil {
		t.Fatal(err)
	}
	reader := func(perms ...string) func(*extsign.Spec) {
		return func(spec *extsign.Spec) {
			spec.Manifest.Requires["host.nodes"] = 1
			spec.Manifest.Requires["host.node_identity"] = 1
			spec.Manifest.Permissions = append(spec.Manifest.Permissions, perms...)
		}
	}
	s.args("acme_fake", "--requires", "table.banners@1-1,host.nodes@1-1,host.node_identity@1-1", "--read-nodes", "acme-salt")
	sv.install(s.fakePackageWith("acme_fake", "1.0.0", reader("read:node_identity", "read:node_inventory")), true)
	got := s.waitResults("acme_fake", 1)[0]
	mac := hmac.New(sha256.New, pluginhost.IdentityKey("acme_fake", "acme-salt"))
	mac.Write([]byte("host\x00" + machine))
	if want := hex.EncodeToString(mac.Sum(nil))[:16]; !got.OK || got.Nodes != 2 || got.Identities != 2 || got.HostH != want {
		t.Fatalf("read = %+v, want 2 nodes, 2 identities and host digest %s", got, want)
	}

	s.args("acme_other", "--requires", "table.banners@1-1,host.nodes@1-1,host.node_identity@1-1", "--read-nodes", "acme-salt")
	sv.install(s.fakePackageWith("acme_other", "1.0.0", reader("read:node_identity")), true)
	refused := s.waitResults("acme_other", 1)[0]
	if refused.OK || refused.Code != "PLUGIN_HOST_NOT_PERMITTED" {
		t.Fatalf("read without read:node_inventory = %+v", refused)
	}
}
