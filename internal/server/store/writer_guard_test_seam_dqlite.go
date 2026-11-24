//go:build linux && dqlite

package store

import (
	"context"

	"gorm.io/gorm"
)

// WithExemptStatementForTest runs fn inside one ordinary transaction of st.
// exempt runs a statement whose context carries the write guard's exemption;
// plain runs one without. The exemption is otherwise unexported: only the
// cluster's own transactions begin with it. Tests of other packages use this
// to show that a statement cannot switch the check off inside a transaction
// that began without it.
func WithExemptStatementForTest(ctx context.Context, st *Store, fn func(exempt, plain func(query string, args ...any) error) error) error {
	return st.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(
			func(query string, args ...any) error {
				return tx.WithContext(withoutWriterGuard(ctx)).Exec(query, args...).Error
			},
			func(query string, args ...any) error { return tx.Exec(query, args...).Error },
		)
	})
}
