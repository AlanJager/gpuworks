//go:build linux && dqlite

package app

import "gpuworks.ai/gpuworks/internal/server/restorehold"

// NoteRestoredClusterID records a replacement cluster id on the restore hold.
// The new id does not isolate the old member and does not open scheduling.
func (a *App) NoteRestoredClusterID(clusterID string) {
	a.disasterMu.Lock()
	defer a.disasterMu.Unlock()
	if a.disaster == nil {
		return
	}
	*a.disaster = NoteClusterIDChange(*a.disaster, clusterID)
}

// ConfirmRestoredMemberIsolated records that the old member is stopped or
// partitioned and drops the durable hold. Scheduling stays closed until the
// controller loop resumes it.
func (a *App) ConfirmRestoredMemberIsolated() error {
	a.disasterMu.Lock()
	defer a.disasterMu.Unlock()
	if err := restorehold.ClearSchedulingIsolationRequired(a.cfg.DataDir); err != nil {
		return err
	}
	if a.disaster != nil {
		*a.disaster = ConfirmMemberIsolated(*a.disaster)
	}
	return nil
}

// restoredSchedulingOpen is the gate runController consults. The lock covers
// the read and the resume write, which happen on the controller goroutine
// while an operator confirms isolation.
func (a *App) restoredSchedulingOpen() bool {
	a.disasterMu.Lock()
	defer a.disasterMu.Unlock()
	return schedulingAllowed(func() *DisasterRecovery { return a.disaster })
}
