package selfinstall

import "errors"

// ErrBackupPreviewMaterialMissing is returned when a native backup directory
// does not contain the identity or Raft metadata a cold snapshot needs.
var ErrBackupPreviewMaterialMissing = errors.New("backup material is missing")

// backupNodeIdentity is one dqlite member recorded in info.yaml or cluster.yaml.
type backupNodeIdentity struct {
	ID      uint64 `json:"id" yaml:"ID"`
	Address string `json:"address" yaml:"Address"`
}
