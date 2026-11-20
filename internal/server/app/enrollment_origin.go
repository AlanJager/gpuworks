//go:build linux && dqlite

package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const maxEnrollmentOriginRequest = 1024

func registerEnrollmentOrigin(router chi.Router, bundle *enrollmentBundle, st *store.Store) {
	registerEnrollmentOriginWithChange(router, bundle, st, nil)
}

func registerEnrollmentOriginWithChange(router chi.Router, bundle *enrollmentBundle, st *store.Store, onChange func()) {
	router.Post(enrollment.ClusterEnrollmentOriginPath, authorizeEnrollmentMember(bundle, st, false, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Origin string `json:"origin"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxEnrollmentOriginRequest+1))
		if err != nil || len(body) > maxEnrollmentOriginRequest {
			http.Error(w, "invalid enrollment origin request", http.StatusBadRequest)
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Origin == "" {
			http.Error(w, "invalid enrollment origin request", http.StatusBadRequest)
			return
		}
		origin, err := url.Parse(request.Origin)
		if err != nil || origin.Hostname() == "" || r.TLS.PeerCertificates[0].VerifyHostname(origin.Hostname()) != nil {
			http.Error(w, "origin is not covered by member certificate", http.StatusForbidden)
			return
		}
		key, err := bundle.trust.VerifyMember(r.TLS.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		identity := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
		changed, err := st.SetClusterEnrollmentOriginForVerifiedKeyChanged(r.Context(), identity, key, request.Origin)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, store.ErrClusterEnrollmentOriginInvalid) {
				status = http.StatusBadRequest
			} else if errors.Is(err, store.ErrClusterAdmissionRejected) || errors.Is(err, store.ErrClusterIdentityConflict) {
				status = http.StatusForbidden
			}
			http.Error(w, "enrollment origin unavailable", status)
			return
		}
		if changed && onChange != nil {
			onChange()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
