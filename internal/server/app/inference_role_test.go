//go:build linux && dqlite

package app

import (
	"context"
	"testing"
)

func TestInferenceRoleChangeIsVisibleWithoutClosingLeadership(t *testing.T) {
	a := &App{}
	if a.inferenceDatabaseRoleChanged(context.Background()) {
		t.Fatal("missing observation looked like a role change")
	}
	a.noteInferenceRole(true)
	if a.inferenceDatabaseRoleChanged(context.Background()) {
		t.Fatal("the first leadership observation was reported as a change")
	}
	a.noteInferenceRole(true)
	if a.inferenceDatabaseRoleChanged(context.Background()) {
		t.Fatal("steady leadership was reported as a role change")
	}
	a.noteInferenceRole(false)
	if !a.inferenceDatabaseRoleChanged(context.Background()) {
		t.Fatal("leadership loss was not visible to inference")
	}
	if a.inferenceDatabaseRoleChanged(context.Background()) {
		t.Fatal("the same role change was reported twice")
	}
	t.Log("a database role change is visible once and does not invent a second one")
}
