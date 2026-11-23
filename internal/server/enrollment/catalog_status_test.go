package enrollment

import (
	"errors"
	"fmt"
	"net/http"
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
