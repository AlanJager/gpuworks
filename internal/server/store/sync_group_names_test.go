package store

import (
	"errors"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

func TestSyncGroupNamesRejectDuplicateCreateAndRename(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateSyncGroup(SyncGroupSpec{Name: "same", Requirements: nodelabel.Requirements{"site": {"a"}}, RateLimitKiB: 12})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateSyncGroup(SyncGroupSpec{Name: "other", Requirements: nodelabel.Requirements{"site": {"b"}}, RateLimitKiB: 24})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() error{
		func() error { _, err := s.CreateSyncGroup(SyncGroupSpec{Name: " same "}); return err },
		func() error {
			_, err := s.UpdateSyncGroup(second.ID, SyncGroupSpec{Name: "same", RateLimitKiB: 99})
			return err
		},
	} {
		err := call()
		var invalid *PullSettingError
		if !errors.As(err, &invalid) || invalid.Field != "name" {
			t.Errorf("duplicate = %v, want name validation error", err)
		}
	}
	groups, err := s.SyncGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range groups {
		if got.ID == first.ID && (got.Name != "same" || got.RateLimitKiB != 12) {
			t.Errorf("first mutated: %+v", got)
		}
		if got.ID == second.ID && (got.Name != "other" || got.RateLimitKiB != 24 || got.Requirements["site"][0] != "b") {
			t.Errorf("refused rename mutated row: %+v", got)
		}
	}
	if _, err := s.UpdateSyncGroup(first.ID, first); err != nil {
		t.Fatalf("unchanged own name: %v", err)
	}
	if _, err := s.CreateSyncGroup(SyncGroupSpec{Name: "SAME"}); err != nil {
		t.Fatalf("names retain case sensitivity: %v", err)
	}
	if err := s.DB().Create(&SyncGroup{Name: "same"}).Error; err == nil {
		t.Error("database accepted duplicate name outside the store")
	} else {
		var invalid *PullSettingError
		if !errors.As(syncGroupWriteError(err), &invalid) || invalid.Reason != "name_duplicate" {
			t.Errorf("typed unique constraint was not mapped to a name error: %v", err)
		}
	}
}

func TestSyncGroupNamesConcurrentCreatesAndRenames(t *testing.T) {
	s := newTestStore(t)
	const workers = 8
	start := make(chan struct{})
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.CreateSyncGroup(SyncGroupSpec{Name: "concurrent"})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
			continue
		}
		var invalid *PullSettingError
		if !errors.As(err, &invalid) || invalid.Field != "name" {
			t.Errorf("concurrent create error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent creates succeeded %d times, want one", success)
	}
	left, err := s.CreateSyncGroup(SyncGroupSpec{Name: "left"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := s.CreateSyncGroup(SyncGroupSpec{Name: "right"})
	if err != nil {
		t.Fatal(err)
	}
	start = make(chan struct{})
	results = make(chan error, 2)
	for _, id := range []uint{left.ID, right.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.UpdateSyncGroup(id, SyncGroupSpec{Name: "renamed"})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success = 0
	for err := range results {
		if err == nil {
			success++
			continue
		}
		var invalid *PullSettingError
		if !errors.As(err, &invalid) || invalid.Field != "name" {
			t.Errorf("concurrent rename error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent renames succeeded %d times, want one", success)
	}
}
