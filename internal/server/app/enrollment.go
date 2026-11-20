//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func verifyEnrollmentState(ctx context.Context, st *store.Store, bundle *enrollmentBundle) error {
	current, err := st.ClusterIdentity(ctx)
	if bundle == nil {
		if errors.Is(err, store.ErrClusterNotInitialized) {
			return nil
		}
		if err != nil {
			return err
		}
		return errors.New("cluster enrollment identity exists but local materials are missing; restore them before starting")
	}
	if err != nil {
		return err
	}
	if current.ClusterID != bundle.info.ClusterID || current.CAFingerprint != bundle.info.CAFingerprint || current.Protocol != bundle.info.Protocol {
		return errors.New("local enrollment materials do not match the database cluster identity")
	}
	return nil
}

func prepareEnrollmentServer(bundle *enrollmentBundle, st *store.Store, signer *auth.TokenSigner, log *slog.Logger, catalog *packages.Store) (*enrollmentServer, error) {
	return prepareEnrollmentServerWithOwner(bundle, st, signer, log, catalog, nil)
}

// prepareEnrollmentServerWithOwner borrows the running closed listener's
// material owner when preparing routes for a later live handler transfer.
func prepareEnrollmentServerWithOwner(bundle *enrollmentBundle, st *store.Store, signer *auth.TokenSigner, log *slog.Logger, catalog *packages.Store, owner *enrollmentServer) (*enrollmentServer, error) {
	if bundle == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handler, err := api.ClusterEnrollmentRouter(ctx, api.ClusterEnrollmentDeps{Store: st, Signer: signer, Issuer: bundle.issuer, PublicURL: bundle.info.PublicURL, StorageTier: bundle.info.StorageTier})
	if err != nil {
		return nil, err
	}
	var receiver *materialReceiver
	var requests *materialRequestTracker
	if owner != nil {
		route, ok := st.ClusterSessionRoute()
		if !ok || route.ID == 0 || owner.materialReceiver == nil || owner.materialRequests == nil || owner.materialReceiver.memberID != route.ID || bundle.dataDir == "" {
			return nil, errors.New("closed enrollment material owner does not match the local member")
		}
		root, err := filepath.EvalSymlinks(bundle.dataDir)
		if err != nil {
			return nil, err
		}
		if owner.materialReceiver.root != filepath.Join(root, "material-replicas") {
			return nil, errors.New("closed enrollment material owner has a different replica root")
		}
		receiver, requests = owner.materialReceiver, owner.materialRequests
	} else if route, ok := st.ClusterSessionRoute(); ok && route.ID != 0 && bundle.dataDir != "" {
		root := filepath.Join(bundle.dataDir, "material-replicas")
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, err
		}
		if err := syncMaterialDir(bundle.dataDir); err != nil {
			return nil, err
		}
		receiver, err = newMaterialReceiver(root, route.ID)
		if err != nil {
			return nil, err
		}
		receiver.log = log
		if err := recoverOrphanedMaterialTransfers(ctx, receiver); err != nil {
			return nil, err
		}
	}
	if receiver != nil && requests == nil {
		requests = newMaterialRequestTracker()
	}
	handler = enrollmentMaterialHandlerWithTrackedReceiver(bundle, st, handler, catalog, receiver, requests)
	server, err := newEnrollmentServer(bundle.info.ListenAddress, accessLogHandler(log, handler), bundle.tls, log)
	if err != nil {
		return nil, err
	}
	server.materialRequests = requests
	server.materialReceiver = receiver
	return server, nil
}

// EnrollmentAddr is empty until enrollment has been explicitly initialized.
func (a *App) EnrollmentAddr() string {
	if a.enrollment == nil {
		return ""
	}
	return a.enrollment.Addr()
}

func (a *App) stopEnrollment() {
	if a.enrollment == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), enrollmentShutdownTimeout)
	defer cancel()
	if err := a.enrollment.Close(ctx); err != nil {
		a.log.Warn("enrollment HTTPS forced shutdown", "err", err)
	}
	_ = a.enrollment.Wait()
}
