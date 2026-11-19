//go:build linux || darwin

package storagemount

import (
	"math"
	"testing"
)

func TestDefaultStatfs(t *testing.T) {
	u, err := defaultStatfs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if u.TotalBytes <= 0 || u.AvailableBytes < 0 || u.AvailableBytes > u.TotalBytes {
		t.Errorf("implausible usage %+v", u)
	}
	if IsNetworkFS(u.FSType) {
		t.Errorf("a temp directory reads as a network filesystem: %+v", u)
	}
	if _, err := defaultStatfs("/nonexistent/dir"); err == nil {
		t.Error("missing path must error")
	}
	// A real manager with the real statfs reports a ready local storage with capacity.
	m := New(Options{})
	st := m.Reconcile(t.Context(), []Spec{{ID: 1, Type: TypeLocal, Path: t.TempDir() + "/x"}})[0]
	if st.State != StateReady || st.TotalBytes <= 0 || !st.Writable {
		t.Errorf("status = %+v", st)
	}
}

func TestBlockBytesSaturates(t *testing.T) {
	if got := BlockBytes(1000, 4096); got != 4096000 {
		t.Fatalf("plain = %d", got)
	}
	if got := BlockBytes(1<<62, 4096); got != math.MaxInt64 {
		t.Fatalf("an NAS that reports near 2^63 bytes = %d, want MaxInt64", got)
	}
	if got := BlockBytes(5, 0); got != 0 {
		t.Fatalf("zero block size = %d", got)
	}
}
