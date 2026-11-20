//go:build linux && dqlite

package cluster

import (
	"testing"

	"github.com/canonical/go-dqlite/v3/client"
)

func TestDialMembersUsesTheRecordedClusterAndKeepsSelf(t *testing.T) {
	self := client.NodeInfo{ID: 7, Address: "192.0.2.7:9001", Role: client.Voter}
	got := dialMembers([]byte("- ID: 7\n  Address: 192.0.2.7:9001\n  Role: 0\n- ID: 8\n  Address: 192.0.2.8:9001\n  Role: 0\n"), self)
	if len(got) != 2 || got[0].Address != "192.0.2.7:9001" || got[1].Address != "192.0.2.8:9001" {
		t.Fatalf("members=%+v", got)
	}
	only := dialMembers([]byte("not: [yaml"), self)
	if len(only) != 1 || only[0] != self {
		t.Fatalf("fallback=%+v", only)
	}
	added := dialMembers([]byte("- ID: 8\n  Address: 192.0.2.8:9001\n"), self)
	if len(added) != 2 || added[1] != self {
		t.Fatalf("self not appended: %+v", added)
	}
}
