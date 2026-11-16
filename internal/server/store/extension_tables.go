package store

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// ErrUnknownNamespace is returned for a namespace the Store was not opened
// with: its tables may not exist, so no handle is handed out for them.
var ErrUnknownNamespace = errors.New("no migration set with this namespace was opened with the store")

// ExtensionTables is how an extension reaches its own tables, the ones its
// MigrationSet created; like CloudLedger, it is the named door to them, and
// the raw handle DB() returns stays for tests.
//
// The contract, which callers keep and the handle does not police: read and
// write only tables whose names start with TablePrefix; reach core data only
// through Store's methods. Write runs on the single writer connection, so a
// function given to Write must not call the Store, Read or another Write (it
// would wait on the connection it holds), and nothing is published on the
// bus for these tables.
type ExtensionTables struct {
	set    MigrationSet
	writer *gorm.DB
	reader *gorm.DB
}

// ExtensionTables is the handle to the tables of namespace, which must be
// one of the sets the Store was opened with (OpenOptions.Extensions).
func (s *Store) ExtensionTables(namespace string) (*ExtensionTables, error) {
	for _, set := range s.extensions {
		if set.Namespace == namespace {
			return &ExtensionTables{set: set, writer: s.db, reader: s.reader()}, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownNamespace, namespace)
}

// Namespace is the set the handle belongs to.
func (t *ExtensionTables) Namespace() string { return t.set.Namespace }

// TablePrefix starts the name of every table the handle may use.
func (t *ExtensionTables) TablePrefix() string { return t.set.TablePrefix }

// Write runs fn in one transaction on the writer: it commits when fn returns
// nil and rolls back otherwise.
func (t *ExtensionTables) Write(fn func(tx *gorm.DB) error) error {
	return t.writer.Transaction(fn)
}

// Read runs fn in one read transaction on the read pool, so its queries see
// one snapshot and never wait on a write in flight. An in-memory database
// has no read pool and reads on the writer.
func (t *ExtensionTables) Read(fn func(db *gorm.DB) error) error {
	return t.reader.Transaction(fn)
}
