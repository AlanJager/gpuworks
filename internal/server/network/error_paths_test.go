package network

import (
	"errors"
	"path"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInvalidErrorMessage(t *testing.T) {
	cases := []struct {
		err  *InvalidError
		want string
	}{
		{&InvalidError{Kind: InvalidName}, "invalid network name"},
		{&InvalidError{Kind: InvalidSelector, Field: "cidr"}, "invalid network selector cidr"},
		{&InvalidError{Kind: InvalidRole, Detail: "why"}, "invalid network role: why"},
		{&InvalidError{Kind: InvalidOverride, Field: "ip", Detail: "why"}, "invalid network override ip: why"},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Fatalf("Error() = %q, want %q", got, c.want)
		}
	}
}

func TestValidGlobRejectsAPatternPathMatchCannotParse(t *testing.T) {
	// path.Match itself must reject the pattern (not merely the whitespace or
	// slash pre-check in validGlob) to exercise that branch.
	const badPattern = "[" // unterminated character class
	if _, err := path.Match(badPattern, ""); err == nil {
		t.Fatalf("test fixture assumption broken: path.Match(%q, \"\") did not error", badPattern)
	}
	if validGlob(badPattern) {
		t.Fatalf("validGlob(%q) = true, want false", badPattern)
	}
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	_, err := svc.CreateNetwork(NetworkSpec{Name: "n", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet, Interface: badPattern})
	assertInvalid(t, err, InvalidSelector, "interface", badPattern)
}

func TestApplyOverridesSkipsBlankHCAs(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	rdma, err := svc.CreateNetwork(NetworkSpec{Name: "rdma-blank", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeHostRDMA})
	if err != nil {
		t.Fatalf("create network: %+v", err)
	}
	node, err := st.RegisterNode("uuid-blank-hca", store.HashToken("b"), "b")
	if err != nil {
		t.Fatalf("register: %+v", err)
	}
	att, err := svc.Attach(rdma.ID, AttachmentSpec{NodeUUID: node.UUID, Overrides: Overrides{HCAs: []string{"  ", "mlx5_0", ""}}})
	if err != nil {
		t.Fatalf("attach: %+v", err)
	}
	if got, want := store.DecodeStringList(att.HCAs), []string{"mlx5_0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hcas = %v, want %v", got, want)
	}
}

func TestUpdateOverridesUnknownNetwork(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	if _, err := svc.UpdateOverrides(999999, 1, Overrides{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %+v, want ErrNotFound", err)
	}
}

func TestNodeNetworksUnknownNode(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	if _, err := svc.NodeNetworks(999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %+v, want ErrNotFound", err)
	}
}

func TestStoreCatalogLoadPropagatesStoreErrors(t *testing.T) {
	st := newStore(t)
	catalog := StoreCatalog{Store: st}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %+v", err)
	}
	if _, _, err := catalog.Load(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("load on a closed store err = %+v, want a closed-database error", err)
	}
}

func TestStateErrorsWhenNodesCannotBeListed(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	if err := st.Close(); err != nil {
		t.Fatalf("close: %+v", err)
	}
	if _, err := svc.State(); err == nil {
		t.Fatal("expected an error from a closed store")
	}
}
