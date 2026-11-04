package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// maintenanceDatabasePolicy preserves a verified installation's service-group
// access. An explicit database outside either recognized configuration is a
// standalone database, whose credentials remain owner-only.
func maintenanceDatabasePolicy(dsn string) (securefile.Policy, error) {
	dbPath, err := store.DatabaseFilePath(dsn)
	if err != nil {
		return securefile.Policy{}, fmt.Errorf("identify maintenance database: %w", err)
	}
	if dbPath == "" {
		return securefile.Policy{}, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return securefile.Policy{}, fmt.Errorf("identify maintenance installation: %w", err)
	}
	for _, dir := range []string{filepath.Dir(executable), selfinstall.DefaultDir} {
		config, err := serverconfig.Load(serverconfig.Path(dir))
		if err != nil {
			// An absent or unreadable configuration grants no group access.
			continue
		}
		layout := selfinstall.NewLayout(dir, config.DataDir, "")
		if dir != selfinstall.DefaultDir && !sameResetPasswordFile(executable, layout.Binary()) {
			continue
		}
		policy, matched, err := maintenancePolicyForDatabase(dsn, layout, selfinstall.SensitiveFilePolicy)
		if err != nil {
			return securefile.Policy{}, fmt.Errorf("identify database permissions: %w; use the installed gpuworks binary or sudo when required", err)
		}
		if matched {
			return policy, nil
		}
	}
	return securefile.Policy{}, nil
}

func maintenancePolicyForDatabase(dsn string, layout selfinstall.Layout, policyFor func(selfinstall.Layout) (securefile.Policy, error)) (securefile.Policy, bool, error) {
	dbPath, err := store.DatabaseFilePath(dsn)
	if err != nil {
		return securefile.Policy{}, false, err
	}
	if !sameResetPasswordFile(dbPath, filepath.Join(layout.DataDir, "gpuworks.db")) {
		return securefile.Policy{}, false, nil
	}
	policy, err := policyFor(layout)
	return policy, true, err
}

func sameResetPasswordFile(left, right string) bool {
	a, errA := os.Stat(left)
	b, errB := os.Stat(right)
	return errA == nil && errB == nil && os.SameFile(a, b)
}
