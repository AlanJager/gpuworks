package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/hwident"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/hostidentity"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The server's host identity answers each known fact salted with the
// plugin's key and leaves unknown ones out; its system UUID gets the same
// digest as the host of a node on the server's machine, so a plugin can
// tell which node that is.
func TestPluginReadsHostIdentityMatchesTheNodeOnTheServersMachine(t *testing.T) {
	st, n := readsStore(t, "host")
	facts := hostidentity.Facts{V: 1, SystemUUID: proto.NormalizeHostSystemUUID(readsHostUUID),
		MachineID: strings.Repeat("ab", 16)}
	r := pluginReads{st: st, host: func() hostidentity.Facts { return facts }}
	key := pluginhost.IdentityKey("acme_backup", "s")
	ans, err := r.HostIdentity(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := ans.Validate(); err != nil {
		t.Fatalf("answer breaks the wire rules: %v", err)
	}
	if ans.BoardSerial != nil || ans.RootFSUUID != nil || ans.MachineID == nil || ans.MachineID.Tail != "abab" {
		t.Fatalf("answer = %+v", ans)
	}
	if want := hwident.Part(key, hwident.KindMachineID, facts.MachineID); ans.MachineID.H != want.H {
		t.Fatalf("machine id = %+v, want %+v", ans.MachineID, want)
	}
	nodes, err := r.NodeIdentity(context.Background(), []uint64{uint64(n.ID)}, key)
	if err != nil || len(nodes) != 1 || nodes[0].Host == nil {
		t.Fatalf("node identity = %+v, %v", nodes, err)
	}
	if *nodes[0].Host != *ans.SystemUUID {
		t.Fatalf("node host %+v, server system UUID %+v", nodes[0].Host, ans.SystemUUID)
	}
	other, _ := r.HostIdentity(context.Background(), pluginhost.IdentityKey("acme_other", "s"))
	if other.SystemUUID.H == ans.SystemUUID.H || other.SystemUUID.Tail != ans.SystemUUID.Tail {
		t.Fatalf("another package = %+v", other.SystemUUID)
	}
	empty, err := pluginReads{st: st, host: func() hostidentity.Facts { return hostidentity.Facts{V: 1} }}.HostIdentity(context.Background(), key)
	if err != nil || empty != (extapi.HostIdentityAnswer{}) {
		t.Fatalf("nothing known = %+v, %v", empty, err)
	}
	if PluginReads(st).(pluginReads).host == nil {
		t.Fatal("the production reads have no host facts")
	}
}

// A compiled module reads the host identity with a salt of 1 to 256
// bytes; one salt always gives the same parts.
func TestHostHostIdentityBoundsTheSalt(t *testing.T) {
	h := &Host{name: "acme"}
	for _, salt := range [][]byte{nil, make([]byte, store.MaxIdentitySalt+1)} {
		if _, err := h.HostIdentity(salt); !errors.Is(err, store.ErrIdentitySalt) {
			t.Errorf("salt of %d bytes = %v", len(salt), err)
		}
	}
	a, err := h.HostIdentity(make([]byte, store.MaxIdentitySalt))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := h.HostIdentity(make([]byte, store.MaxIdentitySalt)); a != b {
		t.Fatalf("one salt gave %+v then %+v", a, b)
	}
	if want := hostidentity.Local().Salted(make([]byte, store.MaxIdentitySalt)); a != want {
		t.Fatalf("HostIdentity = %+v, want %+v", a, want)
	}
}
