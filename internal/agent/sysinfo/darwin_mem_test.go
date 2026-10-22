package sysinfo

import "testing"

func TestDarwinMem(t *testing.T) {
	// The M1 Max the Apple prober fixtures come from: 64 GiB, 76% free.
	total, avail := darwinMem(68719476736, 76, true)
	if total != 67108864 || avail != 51002736 {
		t.Fatalf("darwinMem = %d, %d", total, avail)
	}
	// Without the level the available memory is unknown, not zero-free.
	if total, avail := darwinMem(68719476736, 0, false); total != 67108864 || avail != 0 {
		t.Fatalf("darwinMem without level = %d, %d", total, avail)
	}
	// A nonsense percentage is not trusted.
	if _, avail := darwinMem(68719476736, 250, true); avail != 0 {
		t.Fatalf("darwinMem with level 250 = %d", avail)
	}
	if total, avail := darwinMem(0, 50, true); total != 0 || avail != 0 {
		t.Fatalf("darwinMem(0) = %d, %d", total, avail)
	}
}

func TestStatfsKB(t *testing.T) {
	total, avail := statfsKB(4096, 1000, 250)
	if total != 4000 || avail != 1000 {
		t.Fatalf("statfsKB = %d, %d", total, avail)
	}
}
