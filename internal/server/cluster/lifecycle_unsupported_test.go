//go:build !linux || !dqlite

package cluster

import (
	"errors"
	"testing"
)

func TestStartDatabaseUnavailable(t *testing.T) {
	if db, err := StartDatabase(t.Context(), DatabaseConfig{}); db != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v %v", db, err)
	}
	if db, err := OpenDatabase(t.Context(), DatabaseConfig{}); db != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v %v", db, err)
	}
}
