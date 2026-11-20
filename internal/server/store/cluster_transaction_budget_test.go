package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Every cluster transaction keeps the interactive 5s cap even under a longer
// caller deadline, and a shorter caller deadline still bounds it.
func TestClusterTransactionKeepsItsCapUnderALongCallerDeadline(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "budget.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	remaining := func(ctx context.Context) time.Duration {
		var left time.Duration
		if err := st.clusterTransaction(ctx, func(tx *gorm.DB) error {
			deadline, ok := tx.Statement.Context.Deadline()
			if !ok {
				t.Fatal("cluster transaction has no deadline")
			}
			left = time.Until(deadline)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return left
	}
	long, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	if left := remaining(long); left > 5*time.Second {
		t.Fatalf("default budget=%v, want at most 5s", left)
	}
	short, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if left := remaining(short); left > time.Second {
		t.Fatalf("cap extended past caller deadline: %v", left)
	}
}
