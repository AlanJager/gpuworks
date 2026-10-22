package network

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// newStore opens a fresh in-memory store, unique per test name.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatalf("open store: %+v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// reportInventory stores a node's network inventory as its agent would.
func reportInventory(t *testing.T, st *store.Store, uuid string, inv proto.NetInventory) {
	t.Helper()
	if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &inv}); err != nil {
		t.Fatalf("update node status for %s: %+v", uuid, err)
	}
}

// failCatalog is a Catalog whose Load always fails, to prove a caller does or
// does not consult it.
type failCatalog struct {
	err       error
	dedicated bool
}

func (c failCatalog) Load() ([]store.Network, []store.NetworkAttachment, error) {
	return nil, nil, c.err
}

func (c failCatalog) Dedicated() bool { return c.dedicated }

// assertInvalid fails the test unless err is an *InvalidError with exactly
// this Kind, Field and Value.
func assertInvalid(t *testing.T, err error, kind, field, value string) *InvalidError {
	t.Helper()
	var ierr *InvalidError
	if !errors.As(err, &ierr) {
		t.Fatalf("err = %+v, want *InvalidError", err)
	}
	if ierr.Kind != kind || ierr.Field != field || ierr.Value != value {
		t.Fatalf("invalid error = %+v, want Kind=%q Field=%q Value=%q", ierr, kind, field, value)
	}
	return ierr
}
