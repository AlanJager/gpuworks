package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A server that never initialised or joined a cluster answers as a single
// server in every build, and a module cannot ask without a probe bound.
func TestHostClusterObservationOfASingleServer(t *testing.T) {
	h := &Host{name: "probe", cfg: Config{DataDir: t.TempDir()}}
	if _, err := h.ClusterObservation(context.Background(), 0); !errors.Is(err, ErrClusterObservationTimeout) {
		t.Fatalf("no timeout = %v, want ErrClusterObservationTimeout", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.ClusterObservation(canceled, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v, want context.Canceled", err)
	}
	before := time.Now()
	got, err := h.ClusterObservation(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Clustered || got.ObservedAt.Before(before) || len(got.Members) != 0 || got.LeaderID != 0 ||
		got.Shortfall != (ClusterShortfall{}) || got.FaultTolerance != "" || got.Forming {
		t.Fatalf("single server observation = %+v", got)
	}
}
