//go:build linux && dqlite

package app

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// A receiver that could not commit its own grant in time answers the pinned
// HEAD probe with a labelled 503: it inspected nothing. The sender must take
// that as an unavailable peer. Re-sending the bytes would count as a transfer
// and close the sender's own business although nothing changed.
func TestProbeRefusalWithoutAVerdictSendsNothingAndChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		serve       func(http.ResponseWriter, *http.Request) bool
		unavailable bool
	}{
		{"labelled 503", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != http.MethodHead {
				return false
			}
			w.Header().Set(materialRefusalHeader, materialRefusalAuthorization)
			http.Error(w, "material authorization unavailable", http.StatusServiceUnavailable)
			return true
		}, true},
		{"unlabelled 503", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != http.MethodHead {
				return false
			}
			http.Error(w, "not serving", http.StatusServiceUnavailable)
			return true
		}, false},
		{"403 with label", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != http.MethodHead {
				return false
			}
			w.Header().Set(materialRefusalHeader, materialRefusalAuthorization)
			http.Error(w, "forbidden", http.StatusForbidden)
			return true
		}, false},
		{"200 naming another member", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != http.MethodHead {
				return false
			}
			w.Header().Set("X-Gpuworks-Material-Member", "8")
			w.WriteHeader(http.StatusOK)
			return true
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			item := f.put([]byte("replica the receiver already holds"))
			source := filepath.Join(t.TempDir(), "source")
			if err := os.WriteFile(source, []byte("replica the receiver already holds"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := f.store.RecordMaterialRemoteReceipt(t.Context(), item.Digest, 7, item.Bytes); err != nil {
				t.Fatal(err)
			}
			uploads := 0
			peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					uploads++
				}
				if !tc.serve(w, r) {
					f.handler.ServeHTTP(w, r)
				}
			}))
			peer.TLS = f.server.TLS.Clone()
			peer.StartTLS()
			defer peer.Close()
			err := probeRemoteMaterialReplica(t.Context(), f.client, peer.URL, item.Digest, item.Bytes, 7)
			if err == nil || peerUnreachable(err) != tc.unavailable {
				t.Fatalf("probe error %v: unavailable=%v, want %v", err, peerUnreachable(err), tc.unavailable)
			}
			changes := 0
			transferred, err := ensureRemoteMaterialReplica(t.Context(), f.store, f.client, peer.URL, source, item.Digest, item.Bytes, 7, func() { changes++ })
			if tc.unavailable {
				if err == nil || !peerUnreachable(err) || transferred || uploads != 0 || changes != 0 {
					t.Fatalf("transferred=%v uploads=%d changes=%d err=%v; want an unavailable peer, nothing sent, nothing changed", transferred, uploads, changes, err)
				}
				if materialReplicationFailureInvalidates(fmt.Errorf("replicate manifest to member 7: %w", err)) {
					t.Fatalf("an uninspected probe refusal invalidates: %v", err)
				}
				return
			}
			// A verdict or an unexplained refusal still repairs, and a repair
			// that sent bytes still reports the change.
			if uploads == 0 {
				t.Fatalf("a probe verdict sent nothing: transferred=%v err=%v", transferred, err)
			}
			if err == nil && changes != 1 {
				t.Fatalf("a completed repair reported %d changes, want 1", changes)
			}
		})
	}
}

// The peer call that breaks before any response says nothing about material.
func TestBrokenPeerExchangeIsAnUnavailablePeer(t *testing.T) {
	wrap := func(cause error) error {
		return fmt.Errorf("probe material member 2: %w", &url.Error{Op: "Post", URL: "https://member/_cluster", Err: cause})
	}
	for _, tc := range []struct {
		name        string
		err         error
		unavailable bool
	}{
		{"connection closed before the response", wrap(io.EOF), true},
		{"connection broken inside the response", wrap(fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", io.ErrUnexpectedEOF)), true},
		{"peer certificate not trusted", wrap(x509.UnknownAuthorityError{}), false},
		{"peer identity refused by the pinned verifier", wrap(errors.New("member key is not authorized for transport")), false},
		{"EOF outside a peer request", fmt.Errorf("read manifest: %w", io.ErrUnexpectedEOF), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := peerUnreachable(tc.err); got != tc.unavailable {
				t.Fatalf("unavailable=%v want %v: %v", got, tc.unavailable, tc.err)
			}
			if got := materialReplicationFailureInvalidates(tc.err); got == tc.unavailable {
				t.Fatalf("invalidates=%v for unavailable=%v: %v", got, tc.unavailable, tc.err)
			}
		})
	}
}
