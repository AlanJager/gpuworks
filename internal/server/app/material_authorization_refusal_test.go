//go:build linux && dqlite

package app

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func unavailableAuthorization() error {
	response := statusResponse(http.StatusServiceUnavailable)
	response.Header.Set(materialRefusalHeader, materialRefusalAuthorization)
	return fmt.Errorf("replicate manifest to member 2: %w", materialStatusError(response, []byte("material authorization unavailable")))
}

// The field 503s were all "material authorization unavailable": the receiver
// could not commit its own grant in time and never inspected the bytes. That
// refusal, and only that one, is an unavailable peer.
func TestMaterialAuthorizationUnavailableIsAnUnavailablePeer(t *testing.T) {
	labelled := func(status int, value string) *http.Response {
		response := statusResponse(status)
		if value != "" {
			response.Header.Set(materialRefusalHeader, value)
		}
		return response
	}
	for _, tc := range []struct {
		name        string
		response    *http.Response
		unavailable bool
	}{
		{"503 authorization unavailable", labelled(http.StatusServiceUnavailable, materialRefusalAuthorization), true},
		{"503 without label", labelled(http.StatusServiceUnavailable, ""), false},
		{"503 unknown label", labelled(http.StatusServiceUnavailable, "bytes-differ"), false},
		{"403 with label", labelled(http.StatusForbidden, materialRefusalAuthorization), false},
		{"422 with label", labelled(http.StatusUnprocessableEntity, materialRefusalAuthorization), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, refusal := range []error{
				materialStatusError(tc.response, []byte("material authorization unavailable")),
				newMaterialReceiverStatus(tc.response, fmt.Sprintf("manifest receiver rejected current bytes or identity: HTTP %d", tc.response.StatusCode)),
			} {
				err := fmt.Errorf("probe material member 2: %w", refusal)
				if got := peerUnreachable(err); got != tc.unavailable {
					t.Fatalf("unavailable=%v want %v: %v", got, tc.unavailable, err)
				}
				if got := materialReplicationFailureInvalidates(err); got == tc.unavailable {
					t.Fatalf("invalidates=%v for unavailable=%v: %v", got, tc.unavailable, err)
				}
			}
		})
	}
}

func TestJoinedMaterialFailureInvalidatesThroughAnyBranch(t *testing.T) {
	unavailable := unavailableAuthorization()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"two unavailable peers", errors.Join(unavailable, unavailableAuthorization()), false},
		{"wrapped unavailable join", fmt.Errorf("run: %w", errors.Join(unavailable, unavailable)), false},
		{"unavailable and integrity", errors.Join(unavailable, errors.New("material digest differs")), true},
		{"unavailable and unlabelled 503", errors.Join(unavailable, materialStatusError(statusResponse(http.StatusServiceUnavailable), nil)), true},
		{"nested integrity", errors.Join(unavailable, fmt.Errorf("peer 3: %w", errors.Join(unavailable, errors.New("manifest changed")))), true},
		{"unavailable and cancellation", fmt.Errorf("peer: %w", errors.Join(unavailable, errors.New("context canceled"))), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := materialReplicationFailureInvalidates(tc.err); got != tc.want {
				t.Fatalf("invalidates=%v want %v: %v", got, tc.want, tc.err)
			}
			// The per-member replication loop skips an unavailable peer; one
			// unavailable branch must not let it skip another branch's verdict.
			if got := peerUnreachable(tc.err); got == tc.want {
				t.Fatalf("unavailable=%v for invalidating=%v: %v", got, tc.want, tc.err)
			}
		})
	}
}
