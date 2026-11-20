//go:build linux && dqlite

package app

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestLocalMaterialMemberStaysAdmittedWhenAPeerRestarts(t *testing.T) {
	members := []store.ClusterTransportMember{
		{ID: 1, EnrollmentOrigin: "http://a:8800"},
		{ID: 2, EnrollmentOrigin: "http://b:8800"},
	}
	if localMaterialMemberAbsent(1, "http://a:8800", members) {
		t.Fatal("local member was treated as removed")
	}
	if !localMaterialMemberAbsent(1, "http://a:8800", members[1:]) {
		t.Fatal("removed local member kept admission")
	}
	if !localMaterialMemberAbsent(1, "http://moved:8800", members) {
		t.Fatal("changed local origin kept admission")
	}
}
