package app

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/engine"
)

type legacyTerminalDriver struct {
	imagePullTestDriver
	calls int
}

func (d *legacyTerminalDriver) ExecPTY(context.Context, string, []string) ([]string, error) {
	d.calls++
	return []string{"legacy-shell"}, nil
}

type ownedTerminalDriver struct {
	legacyTerminalDriver
	calls  int
	err    error
	closed bool
}

func (d *ownedTerminalDriver) ExecPTYSession(context.Context, string, []string) ([]string, func(context.Context) error, error) {
	d.calls++
	if d.err != nil {
		return nil, nil, d.err
	}
	return []string{"owned-shell"}, func(context.Context) error { d.closed = true; return nil }, nil
}

func TestNodeTerminalUsesOwnedLifecycleWithoutErrorFallback(t *testing.T) {
	driver := &ownedTerminalDriver{}
	node := &NodeRuntime{engine: engine.New(driver, nil, nil, t.TempDir())}
	argv, closeSession, err := node.ContainerTerminalSession(context.Background(), "owned", nil)
	if err != nil || len(argv) != 1 || argv[0] != "owned-shell" || closeSession == nil || driver.calls != 1 || driver.legacyTerminalDriver.calls != 0 {
		t.Fatalf("owned lifecycle was bypassed: argv=%v err=%v", argv, err)
	}
	if err = closeSession(context.Background()); err != nil || !driver.closed {
		t.Fatalf("remote owner not closed: %v", err)
	}
	driver.err = errors.New("prepare denied")
	argv, closeSession, err = node.ContainerTerminalSession(context.Background(), "owned", nil)
	if err == nil || argv != nil || closeSession != nil || driver.legacyTerminalDriver.calls != 0 {
		t.Fatalf("prepare failure fell back: argv=%v err=%v", argv, err)
	}
	legacy := &legacyTerminalDriver{}
	node.engine = engine.New(legacy, nil, nil, t.TempDir())
	argv, closeSession, err = node.ContainerTerminalSession(context.Background(), "owned", nil)
	if err != nil || closeSession != nil || len(argv) != 1 || argv[0] != "legacy-shell" || legacy.calls != 1 {
		t.Fatalf("legacy runtime changed: argv=%v err=%v", argv, err)
	}
}
