package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// ErrExtensionSchemaNotApplied means an established multi-member database
// lacks an extension step this server includes. One member must not change a
// replicated schema on its own while it rejoins.
var ErrExtensionSchemaNotApplied = errors.New("extension schema is not applied to the established cluster database")

// OpenRuntimeWith is OpenRuntime with OpenWith's options: the file policies
// guard the directory that holds the runtime database, and the extension
// schemas run after the core migrations.
func OpenRuntimeWith(ctx context.Context, path string, b *bus.Bus, o OpenOptions) (*Store, error) {
	if err := validateMigrationSets(o.Extensions); err != nil {
		return nil, err
	}
	return openRuntimeWith(ctx, path, b, false, func(ctx context.Context, path string, b *bus.Bus, _ bool) (*Store, error) {
		return o.Backend.runtimeDatabase().openWith(ctx, path, b, o)
	})
}

// OpenExpandedRuntimeWith is OpenExpandedRuntime for a server with
// extensions. It applies nothing: the extension schemas must already be in
// the replicated database, or the open is refused.
func OpenExpandedRuntimeWith(ctx context.Context, path string, b *bus.Bus, o OpenOptions) (*Store, error) {
	if err := validateMigrationSets(o.Extensions); err != nil {
		return nil, err
	}
	if err := checkRuntimeDirectoryPolicy(path, o.Policies...); err != nil {
		return nil, err
	}
	st, err := OpenExpandedRuntime(ctx, path, b, o.Backend)
	if err != nil {
		return nil, err
	}
	if err := st.AdoptExtensionSchemas(ctx, o.Extensions); err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return st, nil
}

// AdoptExtensionSchemas lets a Store that was bound to an established member
// (OpenExpandedRuntime, OpenOwnedJoinedMemberStore) hand out extension
// tables. It only reads: every step of every set must be recorded under its
// released name. Call it before the Store serves requests.
func (s *Store) AdoptExtensionSchemas(ctx context.Context, sets []MigrationSet) error {
	if err := validateMigrationSets(sets); err != nil {
		return err
	}
	if err := checkNamespacesApplied(s.db.WithContext(ctx), sets); err != nil {
		return err
	}
	// A member without a module the cluster's database holds would serve
	// the cluster without it; a single server only warns about that.
	if _, err := appliedNamespacePositions(s.db.WithContext(ctx), sets); err != nil {
		return err
	}
	s.extensions = append([]MigrationSet(nil), sets...)
	return nil
}

func checkNamespacesApplied(db *gorm.DB, sets []MigrationSet) error {
	if len(sets) == 0 {
		return nil
	}
	if !db.Migrator().HasTable(&NamespaceMigration{}) {
		return fmt.Errorf("%w: namespace %s has no recorded step", ErrExtensionSchemaNotApplied, sets[0].Namespace)
	}
	for _, set := range sets {
		var rows []NamespaceMigration
		if err := db.Where("namespace = ?", set.Namespace).Order("version").Find(&rows).Error; err != nil {
			return err
		}
		recorded := make(map[int]string, len(rows))
		for _, row := range rows {
			recorded[row.Version] = row.Name
		}
		highest := 0
		for _, m := range set.Migrations {
			highest = m.Version
			name, ok := recorded[m.Version]
			if !ok {
				return fmt.Errorf("%w: namespace %s migration %d (%s)", ErrExtensionSchemaNotApplied, set.Namespace, m.Version, m.Name)
			}
			if name != m.Name {
				return fmt.Errorf("namespace %s: migration %d is recorded as %q, this binary names it %q", set.Namespace, m.Version, name, m.Name)
			}
		}
		for _, row := range rows {
			if row.Version > highest {
				return fmt.Errorf("namespace %s: %w: migration %d (%s) recorded, this binary knows up to %d", set.Namespace, ErrNamespaceTooNew, row.Version, row.Name, highest)
			}
		}
	}
	return nil
}

// checkRuntimeDirectoryPolicy holds the directory of a replicated runtime
// database to the policy OpenWith applies to a SQLite file's directory. The
// native directory beside it stays owner-only under every policy: no group
// member reads Raft segments.
func checkRuntimeDirectoryPolicy(path string, policies ...securefile.Policy) error {
	policy, err := securefile.Select(policies...)
	if err != nil {
		return err
	}
	if path == "" || isInMemoryDSN(path) {
		return nil
	}
	parent := filepath.Dir(path)
	directory, err := os.Open(parent)
	if err != nil {
		return fmt.Errorf("secure database directory %s: %w", parent, err)
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("secure database directory %s: %w", parent, err)
	}
	if err := policy.CheckDirectory(info); err != nil {
		return fmt.Errorf("secure database directory %s: %w", parent, err)
	}
	if _, group := policy.GroupID(); group {
		if err := securefile.CheckDirectoryACL(directory); err != nil {
			return fmt.Errorf("secure database directory %s: %w", parent, err)
		}
	}
	native, err := os.Lstat(path + ".dqlite")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !native.IsDir() || native.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("native database directory %s.dqlite must be a directory only its owner can access", path)
	}
	return nil
}
