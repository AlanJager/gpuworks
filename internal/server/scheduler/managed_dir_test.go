package scheduler

import "testing"

// The managed directories' disk is checked like the runtime disk: refused
// below the need, admitted at it, and never refused on a disk the node does
// not report.
func TestScheduleChecksTheManagedDirectoryDisk(t *testing.T) {
	node := NodeSnapshot{UUID: "dirs", DtypeSupported: true, ManagedDirDiskAvailBytes: 100 << 30, GPUs: []GPU{gpu(0, 80, true, 0)}}
	req := Request{WeightBytes: 1 << 30, ManagedDirNeedBytes: 450 << 30, GPUMemUtil: 0.9}
	if result := Schedule(req, node); result.OK || result.Message.Code != ReasonInsufficientManagedDirDisk {
		t.Fatalf("result = %+v", result)
	}
	node.ManagedDirDiskAvailBytes = 450 << 30
	if result := Schedule(req, node); !result.OK {
		t.Fatalf("exact fit refused: %+v", result)
	}
	node.ManagedDirDiskAvailBytes = 0
	if result := Schedule(req, node); !result.OK {
		t.Fatalf("unreported disk refused: %+v", result)
	}
	node.ManagedDirDiskAvailBytes = 1 << 30
	req.ManagedDirNeedBytes = 0
	if result := Schedule(req, node); !result.OK {
		t.Fatalf("launch without directories refused: %+v", result)
	}
}
