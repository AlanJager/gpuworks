package store

import "time"

// TerminalSession is the audit record for one web terminal session.
//
// Metadata only: no keystrokes and no output are ever stored. Terminal input is
// exactly where secrets get typed (`export HF_TOKEN=...`, `docker login -p`,
// cloud provider keys), so recording sessions — even "just the command lines",
// since that is where the secret literally lives — would turn this table into a
// cluster-wide plaintext credential store.
//
// BytesIn/BytesOut are the compromise that keeps this useful: they surface an
// anomaly like "someone moved 40 GB off a node" without touching content.
type TerminalSession struct {
	ID       uint   `gorm:"primaryKey"`
	UserID   uint   `gorm:"index"`
	Username string `gorm:"size:64;index"`
	NodeUUID string `gorm:"size:64;index"`
	Target   string `gorm:"size:16"`  // host | container
	Ref      string `gorm:"size:128"` // container id when Target is container
	ClientIP string `gorm:"size:64"`

	StartedAt time.Time  `gorm:"index"`
	EndedAt   *time.Time // nil while the session is still open

	BytesIn     int64
	BytesOut    int64
	CloseReason string `gorm:"size:64"` // client_closed | shell_exited | idle_timeout | ...
}
