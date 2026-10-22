//go:build darwin

package collector

import (
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The parser reads if_msghdr2 by offset so it can be tested on any host; this
// pins those offsets to the kernel's struct as x/sys describes it.
func TestIfMsghdr2OffsetsMatchTheKernelStruct(t *testing.T) {
	var m unix.IfMsghdr2
	for _, c := range []struct {
		name      string
		got, want uintptr
	}{
		{"Index", unsafe.Offsetof(m.Index), ifMsghdr2Index},
		{"Data", unsafe.Offsetof(m.Data), ifMsghdr2Data},
		{"Data.Ierrors", unsafe.Offsetof(m.Data.Ierrors), ifData64Ierrors},
		{"Data.Oerrors", unsafe.Offsetof(m.Data.Oerrors), ifData64Oerrors},
		{"Data.Ibytes", unsafe.Offsetof(m.Data.Ibytes), ifData64Ibytes},
		{"Data.Obytes", unsafe.Offsetof(m.Data.Obytes), ifData64Obytes},
		{"Data.Iqdrops", unsafe.Offsetof(m.Data.Iqdrops), ifData64Iqdrops},
		{"size", unsafe.Sizeof(m), ifMsghdr2MinLength},
	} {
		if c.got != c.want {
			t.Errorf("%s offset = %d, parser uses %d", c.name, c.got, c.want)
		}
	}
	if unix.RTM_IFINFO2 != rtmIfinfo2 {
		t.Errorf("RTM_IFINFO2 = %#x, parser uses %#x", unix.RTM_IFINFO2, rtmIfinfo2)
	}
}

func TestDarwinSourcesReadThisMac(t *testing.T) {
	nets, err := routeInterfaces()
	if err != nil || len(nets) == 0 {
		t.Fatalf("interfaces = %+v, %v", nets, err)
	}
	disks, err := ioregDisks()
	if err != nil || len(disks) == 0 {
		t.Fatalf("disks = %+v, %v", disks, err)
	}
	for name, d := range disks {
		if d.readBytes == 0 {
			t.Errorf("%s has read nothing since boot: %+v", name, d)
		}
	}
	t.Logf("interfaces %v, disks %v", sortedKeys(nets), sortedKeys(disks))
}

func TestIostatCPUReportsAfterOneInterval(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for one iostat interval")
	}
	cpu := &iostatCPU{}
	cpu.latest() // starts iostat
	deadline := time.Now().Add(macIostatInterval + 10*time.Second)
	for time.Now().Before(deadline) {
		if busy, at, ok := cpu.latest(); ok {
			if busy < 0 || busy > 100 || time.Since(at) > time.Minute {
				t.Fatalf("busy %v at %v", busy, at)
			}
			t.Logf("cpu busy %.0f%%", busy)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no CPU reading within one interval")
}
