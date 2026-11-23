package enrollment

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestStatusErrorKeepsItsMessageAndTellsUnavailableApart(t *testing.T) {
	err := fmt.Errorf("receive cluster catalog: %w", &StatusError{Code: http.StatusServiceUnavailable})
	if got, want := err.Error(), "receive cluster catalog: member request rejected: HTTP 503"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	var status *StatusError
	if !errors.As(err, &status) || !status.Unavailable() {
		t.Fatalf("a 503 must read as unavailable: %v", err)
	}
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway} {
		if (&StatusError{Code: code}).Unavailable() {
			t.Errorf("HTTP %d must not read as unavailable", code)
		}
	}
}

// The status a member endpoint answers reaches the caller as a StatusError,
// for the index and for a package: the join retries on exactly that.
func TestCatalogRequestsReportTheEndpointStatus(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusForbidden} {
		f := endpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "catalog references unavailable", code)
		}))
		cert, key := catalogMember(t, f)
		_, indexErr := f.client(t).FetchCatalogIndex(t.Context(), cert, key)
		_, packageErr := f.client(t).FetchCatalogPackage(t.Context(), cert, key, strings.Repeat("a", 64))
		for name, err := range map[string]error{"index": indexErr, "package": packageErr} {
			var status *StatusError
			if !errors.As(err, &status) || status.Code != code || status.Unavailable() != (code == http.StatusServiceUnavailable) {
				t.Errorf("%s answered HTTP %d: err = %v", name, code, err)
			}
		}
	}
}
