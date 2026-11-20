//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The joining member receives authenticated material without opening the
// ordinary App. The running Raft session owns the SQL pools and listener;
// requests still need a committed quorum grant before reading or writing.
func (s *ClusterRaftSession) startMaterialListener(ctx context.Context, cfg Config) error {
	if s == nil || s.db == nil {
		return errors.New("material listener requires a running join session")
	}
	if s.info.EnrollmentOrigin == "" {
		return nil
	}
	if s.info.EnrollmentListen == "" || s.materialServer != nil {
		return errors.New("joining material listener is missing or already started")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return err
	}
	root = filepath.Join(root, "material-replicas")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err := syncMaterialDir(filepath.Dir(root)); err != nil {
		return err
	}
	receiver, err := newMaterialReceiver(root, s.db.ID)
	if err != nil {
		return err
	}
	if err := recoverOrphanedMaterialTransfers(ctx, receiver); err != nil {
		return err
	}
	ca := []byte(s.snapshot.Secrets.CACertificatePEM)
	pair, err := tls.X509KeyPair(append([]byte(s.snapshot.CertificatePEM), ca...), s.key)
	if err != nil {
		return err
	}
	pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	trust, err := identity.NewTrust(ca, s.info.ClusterID, s.info.CAFingerprint, time.Now().UTC())
	if err != nil {
		return err
	}
	config, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ca)
	if err != nil {
		return err
	}
	st, err := store.OpenJoinedMemberStore(ctx, s.db, bus.New())
	if err != nil {
		return err
	}
	current, err := st.ClusterIdentity(ctx)
	if err != nil || current.ClusterID != s.info.ClusterID || current.CAFingerprint != s.info.CAFingerprint || current.Protocol != s.info.Protocol {
		return errors.Join(errors.New("joining material listener identity differs from replicated database"), err)
	}
	bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: s.info.ClusterID, CAFingerprint: s.info.CAFingerprint, Protocol: s.info.Protocol}, trust: trust, tls: config}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			next.ServeHTTP(w, r)
		})
	})
	requests := newMaterialRequestTracker()
	registerEnrollmentMaterialReceiver(router, bundle, st, receiver, requests)
	server, err := newEnrollmentServer(s.info.EnrollmentListen, router, config, nil)
	if err != nil {
		return err
	}
	server.materialRequests = requests
	if err := server.Start(context.Background()); err != nil {
		return err
	}
	s.materialServer = server
	return nil
}
