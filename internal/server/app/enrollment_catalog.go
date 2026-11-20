//go:build linux && dqlite

package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/catalogasset"
)

// Each request gets a new quorum-backed grant. The index is a point-in-time
// reference snapshot, not a freeze of resource publication or member readiness.
func registerEnrollmentCatalog(router chi.Router, bundle *enrollmentBundle, st *store.Store, catalog *packages.Store) {
	router.Post(enrollment.CatalogPackagesPath, authorizeEnrollmentMaterial(bundle, st, func(w http.ResponseWriter, r *http.Request) {
		digests, err := st.CompleteCatalogPackageReferences(r.Context())
		if err != nil {
			http.Error(w, "catalog references unavailable", http.StatusServiceUnavailable)
			return
		}
		if digests == nil {
			digests = []string{}
		}
		raw, err := json.Marshal(enrollment.CatalogPackageIndex{ClusterID: bundle.info.ClusterID, Protocol: bundle.info.Protocol, Digests: digests})
		if err != nil || len(raw) > 1<<20 {
			http.Error(w, "catalog index exceeds transfer limit", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	router.Post(enrollment.CatalogPackagesPath+"/{digest}", authorizeEnrollmentMaterial(bundle, st, func(w http.ResponseWriter, r *http.Request) {
		digest := chi.URLParam(r, "digest")
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != digest {
			http.Error(w, "invalid catalog digest", http.StatusBadRequest)
			return
		}
		refs, err := st.CompleteCatalogPackageReferences(r.Context())
		if err != nil {
			http.Error(w, "catalog references unavailable", http.StatusServiceUnavailable)
			return
		}
		if !slices.Contains(refs, digest) {
			http.Error(w, "catalog package not referenced", http.StatusNotFound)
			return
		}
		if catalog == nil {
			http.Error(w, "catalog package store unavailable", http.StatusServiceUnavailable)
			return
		}
		// Fully validate before sending headers: missing/corrupt source data must
		// never look like a successful (but truncated) archive transfer.
		var archive catalogTransferBuffer
		if err := catalog.Pack(digest, &archive); err != nil {
			http.Error(w, "catalog package unavailable", http.StatusServiceUnavailable)
			return
		}
		sum := sha256.Sum256(archive.Bytes())
		if hex.EncodeToString(sum[:]) != digest {
			http.Error(w, "catalog package changed during transfer", http.StatusServiceUnavailable)
			return
		}
		if r.Context().Err() != nil {
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.Itoa(archive.Len()))
		_, _ = w.Write(archive.Bytes())
	}))
}

type catalogTransferBuffer struct{ bytes.Buffer }

func (b *catalogTransferBuffer) Write(p []byte) (int, error) {
	if int64(b.Len())+int64(len(p)) > catalogasset.MaxPackageBytes {
		return 0, errors.New("catalog package exceeds transfer limit")
	}
	return b.Buffer.Write(p)
}
