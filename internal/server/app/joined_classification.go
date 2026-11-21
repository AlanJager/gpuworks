//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type joinedStoreClassifier interface {
	OpenClassifiedStore(context.Context, *bus.Bus) (*store.Store, bool, error)
}

// classifyJoinedStore transfers the original session only once. A failed
// quorum wait leaves that session owned by the App for retry or Close.
func (a *App) classifyJoinedStore(ctx context.Context) error {
	if a.store != nil || a.joinedRaft == nil {
		return nil
	}
	if candidate, ok := a.joinedRaft.(interface{ AwaitingMembershipProof() bool }); ok && candidate.AwaitingMembershipProof() {
		// A sparse candidate owns no Raft process. Keep health available until
		// cancellation; the existing SQL classifier cannot supply its proof.
		<-ctx.Done()
		return ctx.Err()
	}
	classifier, ok := a.joinedRaft.(joinedStoreClassifier)
	if !ok {
		// Existing closed-listener fixtures own only Close. Every production
		// waiting native session implements classification.
		return nil
	}
	b := a.bus
	if b == nil {
		b = bus.New()
	}
	announced := false
	for {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, legacy, err := classifier.OpenClassifiedStore(attempt, b)
		attemptErr := attempt.Err()
		cancel()
		// Native leader discovery can discard its expired request context.
		// Restore only an actual deadline while retaining the original error.
		if err != nil && errors.Is(attemptErr, context.DeadlineExceeded) {
			err = errors.Join(err, attemptErr)
		}
		if err == nil {
			if st == nil {
				return errors.New("joined classification returned no owned Store")
			}
			// Even cancellation after successful transfer cannot discard the
			// Store: Close now owns it, rather than a replacement runtime.
			st.SetLogger(a.log)
			a.store = st
			a.legacyClosed = legacy
			if !legacy {
				// The classified store opened without this server's extension
				// schemas. A member applies none: they must already be there.
				if err := a.adoptExtensionSchemas(ctx); err != nil {
					return err
				}
			}
			if err := a.installClosedBusinessWriteBarrier(); err != nil {
				return err
			}
			return ctx.Err()
		}
		if ctx.Err() != nil {
			return err
		}
		if errors.Is(err, store.ErrSchemaUpgradePending) {
			// This executable is ahead of the schema. The member keeps
			// serving the database with its business closed, says which
			// executable it runs, and asks again until the cluster upgrade
			// has applied the migrations.
			if !a.waitForClusterUpgrade(ctx, &announced) {
				return ctx.Err()
			}
			continue
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
}

// waitForClusterUpgrade reports this member's executable for the cluster
// upgrade and waits one interval. It returns false when ctx ended.
func (a *App) waitForClusterUpgrade(ctx context.Context, announced *bool) bool {
	if a.businessHTTP != nil {
		a.businessHTTP.clusterUpgrade.Store(true)
	}
	if !*announced {
		*announced = true
		a.log.Info("database schema is older than this executable; business stays closed until the cluster upgrade applies the migrations")
	}
	if reporter, ok := a.joinedRaft.(interface {
		ReportClusterUpgradeCapability(context.Context) error
	}); ok {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := reporter.ReportClusterUpgradeCapability(attempt); err != nil && ctx.Err() == nil {
			a.log.Debug("report executable for the cluster upgrade", "err", err)
		}
		cancel()
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// adoptExtensionSchemas records this server's extension schemas on a store
// the member did not open with them.
func (a *App) adoptExtensionSchemas(ctx context.Context) error {
	closed, ok := a.ha.(*memberRuntime)
	if !ok {
		return nil
	}
	return a.store.AdoptExtensionSchemas(ctx, storeOptions(closed.member.cfg, closed.member.policy).Extensions)
}

// prepareJoinedMaterialRuntime observes phase before any deferred startup
// work. The Store APIs still check phase in each actual write transaction.
func (a *App) prepareJoinedMaterialRuntime(ctx context.Context) error {
	if err := a.classifyJoinedStore(ctx); err != nil {
		return err
	}
	if a.legacyClosed || a.store == nil || (a.enrollment != nil && a.enrollment.materialReceiver != nil) {
		return nil
	}
	for {
		phase, err := a.store.ReadMaintenancePhase(ctx)
		if err != nil {
			return err
		}
		if a.businessHTTP != nil {
			// A phase other than serving is a cluster upgrade under way.
			a.businessHTTP.clusterUpgrade.Store(phase.Phase != store.MaintenanceServing)
		}
		if phase.Phase == store.MaintenanceServing {
			break
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := installVerifiedJoinedCatalog(ctx, a.cfg, a.store); err != nil {
		return err
	}
	bundle, err := readEnrollmentBundle(a.cfg.DataDir, false)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, joinErr := os.Lstat(a.cfg.DBPath + ".join")
	if joinErr != nil && !errors.Is(joinErr, os.ErrNotExist) {
		return joinErr
	}
	return a.assembleClosedMemberMaterials(ctx, bundle, joinErr == nil)
}

// installVerifiedJoinedCatalog publishes the packages received during join to
// their consumer directory before the material sender scans current references.
// It does not open business admission or replace a missing material proof.
func installVerifiedJoinedCatalog(ctx context.Context, cfg Config, st *store.Store) (resultErr error) {
	phase, err := st.ReadMaintenancePhase(ctx)
	if err != nil || phase.Phase != store.MaintenanceServing {
		return err
	}
	root, err := os.OpenRoot(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	if err := checkMaterialDataRoot(cfg.DataDir, root); err != nil {
		return err
	}
	if _, err := root.Lstat("gpuworks.db.join"); errors.Is(err, os.ErrNotExist) {
		return nil // The seed has no received join catalog.
	} else if err != nil {
		return err
	}
	join, err := openPlainMaterialDirectory(root, "gpuworks.db.join")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, join.Close()) }()
	if _, err := join.Lstat("catalog-packages"); errors.Is(err, os.ErrNotExist) {
		return nil // Existing members still require authenticated material handoff.
	} else if err != nil {
		return err
	}
	cache, err := openPlainMaterialDirectory(join, "catalog-packages")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, cache.Close()) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	digests, err := st.CompleteCatalogPackageReferences(ctx)
	if err != nil {
		return err
	}
	for _, digest := range digests {
		if _, err := root.Lstat(filepath.Join("catalog-packages", digest)); err == nil {
			consumer, err := openPlainMaterialDirectory(root, "catalog-packages")
			if err != nil {
				return err
			}
			_, loadErr := packages.LoadRootContext(ctx, consumer, digest)
			if err := errors.Join(loadErr, consumer.Close()); err != nil {
				return err
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := cache.Lstat(digest); errors.Is(err, os.ErrNotExist) {
			// References may have changed since receipt. The ordinary material
			// handoff must fill these missing consumer bytes and prove readiness.
			continue
		} else if err != nil {
			return err
		}
		if _, err := packages.LoadRootContext(ctx, cache, digest); err != nil {
			return err
		}
		if err := ensureMaterialParent(root, filepath.Join("catalog-packages", digest)); err != nil {
			return err
		}
		if err := publishJoinedCatalogPackage(ctx, root, cache, cfg.DataDir, digest); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func publishJoinedCatalogPackage(ctx context.Context, root, cache *os.Root, dataDir, digest string) (resultErr error) {
	consumer, err := openPlainMaterialDirectory(root, "catalog-packages")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, consumer.Close()) }()
	source, err := openPlainMaterialDirectory(cache, digest)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	consumerInfo, err := consumer.Stat(".")
	if err != nil {
		return err
	}
	sourceInfo, err := source.Stat(".")
	if err != nil {
		return err
	}
	actual, _, err := packages.PutRootContext(ctx, consumer, source.FS())
	if err != nil {
		return err
	}
	if actual != digest {
		return errors.New("joined catalog package differs from its current reference")
	}
	return errors.Join(checkMaterialDataRoot(dataDir, root),
		checkMaterialOwnedDirectory(root, "catalog-packages", consumerInfo),
		checkMaterialOwnedDirectory(cache, digest, sourceInfo), ctx.Err())
}
