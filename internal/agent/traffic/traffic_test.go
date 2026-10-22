package traffic

import "testing"

func TestStorageSyncCountersAccumulateAndIgnoreEmptyReads(t *testing.T) {
	before := Totals()[RoleStorageSync]
	AddStorageSyncRx(100)
	AddStorageSyncTx(40)
	AddStorageSyncRx(0)
	AddStorageSyncTx(-1)
	after := Totals()[RoleStorageSync]
	if after[0]-before[0] != 100 || after[1]-before[1] != 40 {
		t.Fatalf("grew by %v, want rx 100 tx 40", [2]uint64{after[0] - before[0], after[1] - before[1]})
	}
}
