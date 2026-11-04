package engine

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// TestConcurrentDriverReadsAndSwitchAreRaceFree pins the single-owner rule for
// the runtime driver: Driver() reads the field under the same mutex
// SwitchDriver writes it with, so a logtail reader and a runtime preference
// arriving from the server cannot race.
func TestConcurrentDriverReadsAndSwitchAreRaceFree(t *testing.T) {
	docker := &headlessTestDriver{name: "docker"}
	udocker := &headlessTestDriver{name: "udocker"}
	e := newHermeticEngine(docker, nil, slog.Default(), t.TempDir())
	e.SetDesiredFull(context.Background(), nil)

	stop := make(chan struct{})
	lost := make(chan string, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if d := e.Driver(); d == nil {
					lost <- "active driver disappeared"
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		next := docker
		if i%2 == 1 {
			next = udocker
		}
		if err := e.SwitchDriver(context.Background(), next); err != nil {
			t.Fatalf("switch driver: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	select {
	case msg := <-lost:
		t.Fatal(msg)
	default:
	}
}

// TestConcurrentMountManagerReadsAndSwitchAreRaceFree pins the second field the
// runtime switch owns: the mount manager is rebuilt on every switch while a
// reconcile reads it (and can lazily build it), so both go through Engine.mu.
func TestConcurrentMountManagerReadsAndSwitchAreRaceFree(t *testing.T) {
	docker := &headlessTestDriver{name: "docker"}
	udocker := &headlessTestDriver{name: "udocker"}
	e := newHermeticEngine(docker, nil, slog.Default(), t.TempDir())
	e.SetDesiredFull(context.Background(), nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if e.runtimeMountManager() == nil || e.preStartDir("race") == "" {
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		next := docker
		if i%2 == 1 {
			next = udocker
		}
		if err := e.SwitchDriver(context.Background(), next); err != nil {
			t.Fatalf("switch driver: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}
