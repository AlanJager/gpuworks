package cluster

import (
	"context"
	"errors"
	"testing"
)

func TestDatabaseWaitReadyRejectsUnavailableClosedAndCanceled(t *testing.T) {
	if err := (*Database)(nil).WaitReady(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	called := false
	db := &Database{ready: func(context.Context) error { called = true; return nil }}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := db.WaitReady(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if called {
		t.Fatal("canceled wait invoked native readiness")
	}
	db.closed.Store(true)
	if err := db.WaitReady(t.Context()); err == nil {
		t.Fatal("closed wait succeeded")
	}
	if called {
		t.Fatal("closed wait invoked native readiness")
	}
}
