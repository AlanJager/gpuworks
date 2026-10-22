package store

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// SnapshotTo writes a consistent copy of the live database to dest, which must
// not exist yet. It is how a server that keeps running takes the database
// into a pre-upgrade backup (ADR 0040); the copy needs no write-ahead log.
func (s *Store) SnapshotTo(dest string) error {
	if strings.TrimSpace(dest) == "" {
		return errors.New("snapshot needs a destination")
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("snapshot destination %s already exists", dest)
	}
	return s.db.Exec("VACUUM INTO ?", dest).Error
}
