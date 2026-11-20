package identity

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/filelock"
)

func openPublicationOwner(t *testing.T) (string, TransportPolicy, *RuntimeTransportPolicy) {
	t.Helper()
	path, policy := policyFixture(t)
	policy.SourceRevision = 1
	if err := PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenRuntimeTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	return path, policy, owner
}

func TestRuntimeTransportPublicationsKeepOrderedHighWater(t *testing.T) {
	path, policy, owner := openPublicationOwner(t)
	first := policy
	first.Revision = 0
	first.SourceRevision = 2
	first.AllowedKeys = append(slices.Clone(policy.AllowedKeys), strings.Repeat("c", 64))
	second := first
	second.SourceRevision = 3
	second.AllowedKeys = append(slices.Clone(first.AllowedKeys), strings.Repeat("d", 64))
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := owner.apply(first, func(path string, next TransportPolicy, minimum uint64) (TransportPolicy, error) {
			persisted, err := applyTransportPolicySnapshotLocked(path, next, minimum)
			close(entered)
			<-release
			return persisted, err
		})
		firstDone <- err
	}()
	<-entered
	secondStarted, secondEntered := make(chan struct{}), make(chan struct{})
	go func() {
		close(secondStarted)
		_, err := owner.apply(second, func(path string, next TransportPolicy, minimum uint64) (TransportPolicy, error) {
			close(secondEntered)
			if minimum != 2 {
				return TransportPolicy{}, errors.New("second publication lost first durable revision")
			}
			return applyTransportPolicySnapshotLocked(path, next, minimum)
		})
		secondDone <- err
	}()
	<-secondStarted
	select {
	case <-secondEntered:
		t.Fatal("second publisher overlapped unconfirmed publication")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	released = true
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	current, err := owner.Snapshot()
	if err != nil || current.Revision != 3 || current.SourceRevision != 3 {
		t.Fatalf("activated high water: %#v %v", current, err)
	}
	disk, err := LoadTransportPolicy(path)
	if err != nil || !reflect.DeepEqual(current, disk) {
		t.Fatalf("disk high water differs: %#v %v", disk, err)
	}
	if _, err := owner.Apply(first); err == nil {
		t.Fatal("older publication lowered high water")
	}
	if owner.Authorize(strings.Repeat("d", 64)) != nil {
		t.Fatal("older rejected publication removed latest authority")
	}
}

func TestRuntimeTransportCloseWaitsForPublicationAndReleasesLeaseOnce(t *testing.T) {
	path, policy, owner := openPublicationOwner(t)
	next := policy
	next.Revision = 0
	next.SourceRevision = 2
	next.AllowedKeys = append(slices.Clone(policy.AllowedKeys), strings.Repeat("c", 64))
	originalRelease := owner.release
	closeFailure := errors.New("release outcome marker")
	var releaseCalls atomic.Int32
	owner.release = func() error { releaseCalls.Add(1); return errors.Join(originalRelease(), closeFailure) }
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	applyDone := make(chan error, 1)
	go func() {
		_, err := owner.apply(next, func(path string, next TransportPolicy, minimum uint64) (TransportPolicy, error) {
			persisted, err := applyTransportPolicySnapshotLocked(path, next, minimum)
			close(entered)
			<-release
			return persisted, err
		})
		applyDone <- err
	}()
	<-entered
	closeStarted, closeDone := make(chan struct{}), make(chan error, 1)
	go func() { close(closeStarted); closeDone <- owner.Close() }()
	<-closeStarted
	select {
	case <-closeDone:
		t.Fatal("Close released lease before publication completed")
	case <-time.After(25 * time.Millisecond):
	}
	if _, err := ApplyTransportPolicySnapshot(path, next); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("publication lost runtime lease: %v", err)
	}
	close(release)
	released = true
	if err := <-applyDone; err != nil {
		t.Fatal(err)
	}
	result := <-closeDone
	if !errors.Is(result, closeFailure) || releaseCalls.Load() != 1 {
		t.Fatalf("Close result=%v release calls=%d", result, releaseCalls.Load())
	}
	if owner.Close() != result || releaseCalls.Load() != 1 {
		t.Fatal("Close did not retain its original release result")
	}
	if owner.Authorize(policy.AllowedKeys[0]) == nil {
		t.Fatal("closed owner still authorizes")
	}
	published := false
	if _, err := owner.apply(next, func(string, TransportPolicy, uint64) (TransportPolicy, error) {
		published = true
		return TransportPolicy{}, nil
	}); err == nil || published {
		t.Fatal("closed owner published")
	}
	replacement, err := OpenRuntimeTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if owner.Close() != result {
		t.Fatal("old Close changed its result")
	}
	if _, err := ApplyTransportPolicySnapshot(path, next); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("old Close released replacement lease: %v", err)
	}
}

func TestRuntimeTransportIdenticalRecreatedFileStillConfirmsPublication(t *testing.T) {
	path, policy, owner := openPublicationOwner(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	next := policy
	next.Revision = 0
	unknown := errors.New("identical file confirmation unavailable")
	calls := 0
	if _, err := owner.apply(next, func(path string, next TransportPolicy, minimum uint64) (TransportPolicy, error) {
		calls++
		if _, err := applyTransportPolicySnapshotLocked(path, next, minimum); err != nil {
			return TransportPolicy{}, err
		}
		return TransportPolicy{}, unknown
	}); !errors.Is(err, unknown) || calls != 1 {
		t.Fatalf("identical bytes bypassed confirmation: calls=%d err=%v", calls, err)
	}
	current, err := owner.Snapshot()
	if err != nil || !reflect.DeepEqual(current, policy) {
		t.Fatalf("unknown identical publication changed memory: %#v %v", current, err)
	}
	confirmed, err := owner.Apply(next)
	if err != nil || !reflect.DeepEqual(confirmed, policy) {
		t.Fatalf("identical publication retry: %#v %v", confirmed, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Apply(next); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing disk state was not re-read: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing file silently recreated")
	}
}
