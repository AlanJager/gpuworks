package app

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// normalizeLaunchConfigs brings every stored deployment to the shape the write
// boundary now produces: one complete launch configuration, the user's residual
// arguments, and a catalog snapshot holding only the template's residual.
//
// Rows written before that boundary existed hold the same launch described in
// up to three places at once, and the reader that merged them is gone. Doing
// this once at startup means nothing downstream has to know that two shapes
// ever existed. It runs on every boot and is idempotent, so a row it already
// rewrote is left untouched.
//
// It lives here rather than in store because the merge rules belong to the
// deploy package and store must not depend on a domain package. A failure
// aborts startup: serving from rows whose configuration could not be read would
// mean launching engines with arguments nobody can account for.
func normalizeLaunchConfigs(st *store.Store) error {
	deployments, err := st.ListDeployments()
	if err != nil {
		return fmt.Errorf("list deployments for launch configuration normalization: %w", err)
	}
	for index := range deployments {
		dep := &deployments[index]
		snapshot, hasSnapshot, templateArgs, err := launchConfigSnapshot(dep.CatalogSelectionSnapshot)
		if err != nil {
			return fmt.Errorf("deployment %d catalog selection snapshot: %w", dep.ID, err)
		}
		out, changed, err := deploy.NormalizeStoredLaunchConfig(deploy.StoredLaunchConfig{
			Engine:          dep.Engine,
			QuickParamsJSON: dep.QuickParams,
			ExtraArgsText:   dep.ExtraArgs,
			TemplateArgs:    templateArgs,
		})
		if err != nil {
			return fmt.Errorf("deployment %d launch configuration: %w", dep.ID, err)
		}
		if !changed {
			continue
		}
		snapshotJSON := dep.CatalogSelectionSnapshot
		if hasSnapshot {
			if err := snapshot.SetTemplateArgs(out.TemplateArgs); err != nil {
				return fmt.Errorf("deployment %d catalog selection snapshot: %w", dep.ID, err)
			}
			encoded, err := snapshot.Encode()
			if err != nil {
				return fmt.Errorf("deployment %d catalog selection snapshot: %w", dep.ID, err)
			}
			snapshotJSON = string(encoded)
		}
		if err := st.UpdateDeploymentLaunchConfig(dep.ID, out.QuickParamsJSON, out.ExtraArgsText, snapshotJSON); err != nil {
			return fmt.Errorf("deployment %d launch configuration: %w", dep.ID, err)
		}
	}
	return nil
}

// launchConfigSnapshot reads a catalog selection snapshot and the template
// arguments it carries. The snapshot holds far more than the launch
// configuration, so it is carried as the stored document and only the one field
// this normalizer owns is replaced (deploy.CatalogSelection.SetTemplateArgs).
// The second result says whether the row had a snapshot at all.
func launchConfigSnapshot(raw string) (deploy.CatalogSelection, bool, []string, error) {
	if strings.TrimSpace(raw) == "" {
		return deploy.CatalogSelection{}, false, nil, nil
	}
	snapshot, err := deploy.DecodeCatalogSelection([]byte(raw))
	if err != nil {
		return deploy.CatalogSelection{}, false, nil, err
	}
	return snapshot, true, snapshot.ExtraArgs, nil
}
