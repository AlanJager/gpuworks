package hub

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// setProxies replaces every proxy entry with entries, in their order.
func setProxies(t *testing.T, st *store.Store, entries ...store.PullProxySpec) {
	t.Helper()
	existing, err := st.PullProxies()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range existing {
		if err := st.DeletePullProxy(entry.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range entries {
		if _, err := st.CreatePullProxy(entry, ""); err != nil {
			t.Fatal(err)
		}
	}
}

// everyNode is an entry for every node, chosen for the management node too.
func everyNode(httpProxy, httpsProxy string) store.PullProxySpec {
	return store.PullProxySpec{HTTPProxy: httpProxy, HTTPSProxy: httpsProxy, ControlPlane: true}
}

// forNode is an entry for one node.
func forNode(uuid, httpProxy, httpsProxy string) store.PullProxySpec {
	return store.PullProxySpec{HTTPProxy: httpProxy, HTTPSProxy: httpsProxy, Requirements: nodelabel.Requirements{nodelabel.KeyNode: {uuid}}}
}
