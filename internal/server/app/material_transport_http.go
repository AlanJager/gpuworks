//go:build linux && dqlite

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// This route is registered only when the application supplies a receiver
// bound to its actual local member ID and persistent data directory.
func registerEnrollmentMaterialReceiver(router chi.Router, bundle *enrollmentBundle, st *store.Store, receiver *materialReceiver, requests *materialRequestTracker) {
	authorized := func(emptyBody bool, next http.HandlerFunc) http.HandlerFunc {
		return requests.wrap(authorizeEnrollmentMember(bundle, st, emptyBody, next))
	}
	manifestHandler := func(action string) http.HandlerFunc {
		return authorized(true, func(w http.ResponseWriter, r *http.Request) {
			digest := chi.URLParam(r, "digest")
			if !validMaterialDigest(digest) || strings.TrimSpace(r.Header.Get("Authorization")) != "" {
				http.Error(w, "invalid material manifest verification", http.StatusBadRequest)
				return
			}
			key, err := bundle.trust.VerifyMember(r.TLS.PeerCertificates[0], time.Now().UTC())
			if err != nil {
				http.Error(w, "material source identity rejected", http.StatusForbidden)
				return
			}
			expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
			var consumerRoot *os.Root
			if action == "materialize" || action == "installed" {
				consumerRoot, err = os.OpenRoot(filepath.Dir(receiver.root))
				if err != nil {
					refuseMaterialManifest(w, manifestNotCurrent, err)
					return
				}
				defer consumerRoot.Close()
				if err := checkMaterialDataRoot(filepath.Dir(receiver.root), consumerRoot); err != nil {
					refuseMaterialManifest(w, manifestNotCurrent, err)
					return
				}
			}
			manifest, err := verifyReceivedMaterialManifest(r.Context(), st, receiver, expected, key, digest)
			if err != nil {
				refuseMaterialManifest(w, manifestNotCurrent, err)
				return
			}
			progress := materialManifestProgressReporter(w, r, requests, materialPeerIdleTimeout/4)
			var substageProgress func() error
			if r.Header.Get(materialManifestProgressHeader) == "1" {
				substageProgress = progress
			}
			if err := progress(); err != nil {
				refuseMaterialManifest(w, manifestInterrupted, err)
				return
			}
			if action == "materialize" || action == "installed" {
				if err := checkMaterialConsumerReferences(r.Context(), st, receiver, manifest); err != nil {
					refuseMaterialManifest(w, manifestReferencesNotPortable, err)
					return
				}
				if err := progress(); err != nil {
					refuseMaterialManifest(w, manifestInterrupted, err)
					return
				}
			}
			verifyConsumerReceipt := func() error {
				if _, err := verifyReceivedMaterialManifest(r.Context(), st, receiver, expected, key, digest); err != nil {
					return err
				}
				if err := checkMaterialDataRoot(filepath.Dir(receiver.root), consumerRoot); err != nil {
					return err
				}
				if err := progress(); err != nil {
					return err
				}
				// Replicas do not prove that current consumer paths still hold
				// the bytes installed or inspected before the replica proof.
				if err := checkInstalledMaterialManifestWithProgress(r.Context(), receiver, manifest, substageProgress); err != nil {
					return err
				}
				if err := progress(); err != nil {
					return err
				}
				if err := checkMaterialConsumerReferences(r.Context(), st, receiver, manifest); err != nil {
					return err
				}
				if err := checkMaterialDataRoot(filepath.Dir(receiver.root), consumerRoot); err != nil {
					return err
				}
				return progress()
			}
			changed := manifestChangedInstalling
			var confirm func() error
			if action == "materialize" || action == "installed" {
				// The receipt call is reachable only after this check has
				// returned. Run it again inside the receipt, immediately
				// before the status is committed.
				confirm = verifyConsumerReceipt
				if action == "installed" {
					changed = manifestChangedInstalled
				}
			}
			if action == "materialize" {
				installErr := materializeReceivedManifestWithProgress(r.Context(), receiver, manifest, receiver.onChange, consumerRoot, substageProgress)
				if installErr != nil {
					refuseMaterialManifest(w, manifestInstallFailed, installErr)
					return
				}
				if err := progress(); err != nil {
					refuseMaterialManifest(w, manifestInterrupted, err)
					return
				}
				if err := verifyConsumerReceipt(); err != nil {
					refuseMaterialManifest(w, changed, err)
					return
				}
			} else if action == "installed" {
				if err := checkInstalledMaterialManifestWithProgress(r.Context(), receiver, manifest, substageProgress); err != nil {
					refuseMaterialManifest(w, manifestNotInstalled, err)
					return
				}
				if err := progress(); err != nil {
					refuseMaterialManifest(w, manifestInterrupted, err)
					return
				}
				if err := verifyConsumerReceipt(); err != nil {
					refuseMaterialManifest(w, changed, err)
					return
				}
			}
			writeMaterialManifestReceipt(w, r, requests, manifest.SourceMemberID, receiver.memberID, digest, confirm, changed)
		})
	}
	router.Post(materialManifestPath+"/{digest}/verify", manifestHandler("verify"))
	router.Post(materialManifestPath+"/{digest}/materialize", manifestHandler("materialize"))
	router.Post(materialManifestPath+"/{digest}/installed", manifestHandler("installed"))
	batchHandler := authorized(false, func(w http.ResponseWriter, r *http.Request) {
		serveMaterialBatch(w, r, receiver)
	})
	router.Post(materialBatchPath, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), materialBatchTimeout)
		defer cancel()
		batchHandler(w, r.WithContext(ctx))
	})
	router.Head(materialReplicaPath+"/{digest}", authorized(true, func(w http.ResponseWriter, r *http.Request) {
		digest := chi.URLParam(r, "digest")
		if !validMaterialDigest(digest) || strings.TrimSpace(r.Header.Get("Authorization")) != "" {
			http.Error(w, "invalid material probe", http.StatusBadRequest)
			return
		}
		name := filepath.Join(receiver.root, digest[:2], digest)
		size, err := probeMaterialFile(r.Context(), name, digest)
		if err != nil {
			http.Error(w, "material unavailable", http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Gpuworks-Material-Member", strconv.FormatUint(receiver.memberID, 10))
		w.Header().Set("X-Gpuworks-Material-Digest", digest)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
	}))
	router.Get(materialReplicaPath+"/{digest}", authorized(true, func(w http.ResponseWriter, r *http.Request) {
		digest := chi.URLParam(r, "digest")
		if !validMaterialDigest(digest) || strings.TrimSpace(r.Header.Get("Authorization")) != "" {
			http.Error(w, "invalid material read", http.StatusBadRequest)
			return
		}
		path := filepath.Join(receiver.root, digest[:2], digest)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxMaterialReplicaBytes {
			http.Error(w, "material unavailable", http.StatusNotFound)
			return
		}
		file, err := os.Open(path)
		if err != nil {
			http.Error(w, "material unavailable", http.StatusNotFound)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Gpuworks-Material-Member", strconv.FormatUint(receiver.memberID, 10))
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		_, _ = io.Copy(w, file)
	}))
	router.Post(materialReplicaPath+"/{digest}", authorized(false, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength == 0 || r.ContentLength > maxMaterialReplicaBytes || strings.TrimSpace(r.Header.Get("Authorization")) != "" {
			http.Error(w, "invalid material transfer", http.StatusBadRequest)
			return
		}
		receipt, err := receiver.receive(r.Context(), chi.URLParam(r, "digest"), r.Body)
		if err != nil {
			if receiver.log != nil {
				stage, class := "unknown", "other"
				var failure *materialReceiveError
				if errors.As(err, &failure) {
					stage = failure.stage
				}
				switch {
				case errors.Is(err, context.Canceled):
					class = "canceled"
				case errors.Is(err, context.DeadlineExceeded):
					class = "deadline_exceeded"
				}
				receiver.log.WarnContext(r.Context(), "cluster material receive failed", "stage", stage, "err_class", class)
			}
			http.Error(w, "material transfer could not be persisted", http.StatusUnprocessableEntity)
			return
		}
		path := filepath.Join(receiver.root, receipt.Digest[:2], receipt.Digest)
		if err := st.RecordLocalMaterialReplicaReference(r.Context(), receipt.Digest, receipt.MemberID, path); err != nil {
			http.Error(w, "material transfer could not be indexed", http.StatusServiceUnavailable)
			return
		}
		if r.Context().Err() != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(receipt)
	}))
}

func writeMaterialManifestReceipt(w http.ResponseWriter, r *http.Request, requests *materialRequestTracker, sourceID, memberID uint64, digest string, confirm func() error, changed materialManifestRefusal) {
	w.Header().Set("X-Gpuworks-Material-Source", strconv.FormatUint(sourceID, 10))
	w.Header().Set("X-Gpuworks-Material-Member", strconv.FormatUint(memberID, 10))
	w.Header().Set("X-Gpuworks-Material-Digest", digest)
	// This decision is ordered against owner withdrawal. Response writing may
	// block, so release the lock before writing; the tracker still counts this
	// handler until its write returns.
	requests.mu.Lock()
	interrupted := requests.closed || r.Context().Err() != nil
	requests.mu.Unlock()
	if interrupted {
		refuseMaterialManifest(w, manifestInterrupted, r.Context().Err())
		return
	}
	// Header staging above can block. The consumer check that admitted this
	// receipt has already returned, so read the installed bytes once more
	// before the status is committed.
	if confirm != nil {
		if err := confirm(); err != nil {
			refuseMaterialManifest(w, changed, err)
			return
		}
	}
	requests.mu.Lock()
	interrupted = requests.closed || r.Context().Err() != nil
	requests.mu.Unlock()
	if interrupted {
		refuseMaterialManifest(w, manifestInterrupted, r.Context().Err())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func clearMaterialManifestReceipt(w http.ResponseWriter) {
	w.Header().Del("X-Gpuworks-Material-Source")
	w.Header().Del("X-Gpuworks-Material-Member")
	w.Header().Del("X-Gpuworks-Material-Digest")
}
