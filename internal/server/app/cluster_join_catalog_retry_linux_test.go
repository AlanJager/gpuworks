//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// busySource answers the catalog index as a member that cannot read its
// references yet: unavailable times with a 503, then an empty index.
type busySource struct {
	unavailable int
	failWith    error
	indexCalls  int
}

func (s *busySource) FetchCatalogIndex(context.Context, []byte, []byte) (enrollment.CatalogPackageIndex, error) {
	s.indexCalls++
	if s.failWith != nil {
		return enrollment.CatalogPackageIndex{}, s.failWith
	}
	if s.indexCalls <= s.unavailable {
		return enrollment.CatalogPackageIndex{}, &enrollment.StatusError{Code: http.StatusServiceUnavailable}
	}
	return enrollment.CatalogPackageIndex{Digests: []string{}}, nil
}

func (s *busySource) FetchCatalogPackage(context.Context, []byte, []byte, string) (inferencecatalog.Archive, error) {
	return inferencecatalog.Archive{}, errors.New("the empty index names no package")
}

func quickCatalogRetries(t *testing.T, total time.Duration) {
	t.Helper()
	retryFor, initial, maximum := joinCatalogRetryFor, joinCatalogRetryInitial, joinCatalogRetryMax
	joinCatalogRetryFor, joinCatalogRetryInitial, joinCatalogRetryMax = total, time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { joinCatalogRetryFor, joinCatalogRetryInitial, joinCatalogRetryMax = retryFor, initial, maximum })
}

// A member on slow storage answers the catalog index with 503 for a moment;
// the joining member asks again instead of failing the whole install.
func TestJoinCatalogReceiptRidesOutAnUnavailableSource(t *testing.T) {
	quickCatalogRetries(t, 5*time.Second)
	source := &busySource{unavailable: 3}
	if err := receiveJoinCatalog(t.Context(), t.TempDir(), source, nil, nil); err != nil {
		t.Fatalf("receipt after three 503 answers: %v", err)
	}
	// Three refused, one served, and the closing index read.
	if source.indexCalls != 5 {
		t.Fatalf("index requests = %d, want 5", source.indexCalls)
	}
}

// Asking again is bounded: a source that stays unavailable fails the receipt
// with the status it answered.
func TestJoinCatalogReceiptGivesUpOnASourceThatStaysUnavailable(t *testing.T) {
	quickCatalogRetries(t, 40*time.Millisecond)
	source := &busySource{unavailable: 1 << 30}
	started := time.Now()
	err := receiveJoinCatalog(t.Context(), t.TempDir(), source, nil, nil)
	var status *enrollment.StatusError
	if !errors.As(err, &status) || status.Code != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want the 503 the source answered", err)
	}
	if source.indexCalls < 2 || time.Since(started) > 2*time.Second {
		t.Fatalf("index requests = %d in %v, want several within the retry time", source.indexCalls, time.Since(started))
	}
}

// Only "unavailable right now" is asked again: a refusal, a missing package
// or an unreachable source is the answer.
func TestJoinCatalogReceiptDoesNotRetryOtherFailures(t *testing.T) {
	quickCatalogRetries(t, 5*time.Second)
	for name, failure := range map[string]error{
		"forbidden":   &enrollment.StatusError{Code: http.StatusForbidden},
		"not found":   &enrollment.StatusError{Code: http.StatusNotFound},
		"unreachable": errors.New("dial tcp 10.0.0.1:443: connect: connection refused"),
	} {
		source := &busySource{failWith: failure}
		if err := receiveJoinCatalog(t.Context(), t.TempDir(), source, nil, nil); !errors.Is(err, failure) {
			t.Errorf("%s: err = %v, want %v", name, err, failure)
		}
		if source.indexCalls != 1 {
			t.Errorf("%s: index requests = %d, want 1", name, source.indexCalls)
		}
	}
}

// The join's own deadline ends the waiting, and the error says both why the
// receipt was waiting and that the time ran out.
func TestJoinCatalogReceiptStopsWaitingWhenTheJoinDeadlineEnds(t *testing.T) {
	retryFor, initial, maximum := joinCatalogRetryFor, joinCatalogRetryInitial, joinCatalogRetryMax
	joinCatalogRetryFor, joinCatalogRetryInitial, joinCatalogRetryMax = time.Minute, time.Minute/2, time.Minute/2
	t.Cleanup(func() { joinCatalogRetryFor, joinCatalogRetryInitial, joinCatalogRetryMax = retryFor, initial, maximum })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	source := &busySource{unavailable: 1 << 30}
	err := receiveJoinCatalog(ctx, t.TempDir(), source, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("err = %v, want the deadline and the 503", err)
	}
	if source.indexCalls != 1 {
		t.Fatalf("index requests = %d, want 1", source.indexCalls)
	}
}

// packageSource lists one package and answers its fetch as a member that
// cannot pack it yet: unavailable times with a 503, then with stop.
type packageSource struct {
	unavailable  int
	stop         error
	packageCalls int
}

func (s *packageSource) FetchCatalogIndex(context.Context, []byte, []byte) (enrollment.CatalogPackageIndex, error) {
	return enrollment.CatalogPackageIndex{Digests: []string{strings.Repeat("a", 64)}}, nil
}

func (s *packageSource) FetchCatalogPackage(context.Context, []byte, []byte, string) (inferencecatalog.Archive, error) {
	s.packageCalls++
	if s.packageCalls <= s.unavailable {
		return inferencecatalog.Archive{}, &enrollment.StatusError{Code: http.StatusServiceUnavailable}
	}
	return inferencecatalog.Archive{}, s.stop
}

// The package fetch is asked again as the index is: the answer after the
// 503s, here a refusal, is what the receipt returns.
func TestJoinCatalogReceiptAsksAgainForAPackage(t *testing.T) {
	quickCatalogRetries(t, 5*time.Second)
	refused := &enrollment.StatusError{Code: http.StatusNotFound}
	source := &packageSource{unavailable: 3, stop: refused}
	if err := receiveJoinCatalog(t.Context(), t.TempDir(), source, nil, nil); !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the answer that followed the 503s", err)
	}
	if source.packageCalls != 4 {
		t.Fatalf("package requests = %d, want 4", source.packageCalls)
	}
}
