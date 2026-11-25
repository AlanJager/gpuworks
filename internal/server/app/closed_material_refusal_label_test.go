//go:build linux && dqlite

package app

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A member that is still closed has no receiver: its 503 says nothing about
// the sender's material. Without a label the sender took it for a verdict and
// closed its own business every time a peer restarted or was still joining.
// A refusal of the requester's identity carries no label and stays a verdict.
func TestClosedMaterialStartupLabelsOnlyItsUnavailableRefusals(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, mode := range []string{"active", "busy", "unadmitted", "revoked", "provider-address"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newClosedDiscoveryRouteFixture(t)
			pair := &f.requesterPair
			wantStatus, wantLabel := http.StatusServiceUnavailable, materialRefusalNotStarted
			switch mode {
			case "busy":
				s.err = errors.New("native session is classifying or closing")
			case "unadmitted":
				pair, wantStatus, wantLabel = &f.unknownPair, http.StatusForbidden, ""
			case "revoked":
				now := time.Now()
				s.admissions[1].RevokedAt, wantStatus, wantLabel = &now, http.StatusForbidden, ""
			case "provider-address":
				s.members[0].Address, wantStatus, wantLabel = "127.0.0.2:9910", http.StatusForbidden, ""
			}
			server := startClosedDiscoveryFixture(t, f, s)
			client := f.client(t, pair)
			for _, request := range []struct{ method, path string }{
				{http.MethodPost, materialBatchPath},
				{http.MethodPost, materialReplicaPath + "/" + digest},
				{http.MethodPost, materialManifestPath + "/" + digest + "/verify"},
				{http.MethodPost, materialManifestPath + "/" + digest + "/installed"},
			} {
				response, body, err := closedFactsRouteRequest(t, client, server, request.method, request.path, "")
				if err != nil || response.StatusCode != wantStatus {
					t.Fatalf("%s %s: response=%v body=%s err=%v, want %d", request.method, request.path, response, body, err, wantStatus)
				}
				if got := response.Header.Get(materialRefusalHeader); got != wantLabel {
					t.Fatalf("%s %s: refusal label %q, want %q", request.method, request.path, got, wantLabel)
				}
				refusal := fmt.Errorf("replicate material to member 2: %w", materialStatusError(response, body))
				if got, want := peerUnreachable(refusal), wantLabel != ""; got != want {
					t.Fatalf("%s %s: unavailable peer = %v, want %v", request.method, request.path, got, want)
				}
				if got, want := materialReplicationFailureInvalidates(refusal), wantLabel == ""; got != want {
					t.Fatalf("%s %s: closes the sender = %v, want %v", request.method, request.path, got, want)
				}
			}
		})
	}
}

// The not-started label only ever excuses a 503.
func TestMaterialNotStartedLabelIsAnUnavailablePeerOnlyOn503(t *testing.T) {
	for _, tc := range []struct {
		status      int
		label       string
		unavailable bool
	}{
		{http.StatusServiceUnavailable, materialRefusalNotStarted, true},
		{http.StatusServiceUnavailable, "", false},
		{http.StatusServiceUnavailable, "receiver-not-started-and-more", false},
		{http.StatusForbidden, materialRefusalNotStarted, false},
		{http.StatusConflict, materialRefusalNotStarted, false},
	} {
		response := statusResponse(tc.status)
		if tc.label != "" {
			response.Header.Set(materialRefusalHeader, tc.label)
		}
		err := fmt.Errorf("probe material member 2: %w", newMaterialReceiverStatus(response, "manifest receiver rejected current bytes or identity"))
		if got := peerUnreachable(err); got != tc.unavailable {
			t.Errorf("HTTP %d label %q: unavailable peer = %v, want %v", tc.status, tc.label, got, tc.unavailable)
		}
	}
}
