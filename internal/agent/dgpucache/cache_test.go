package dgpucache

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParseCacheV12(t *testing.T) {
	data := make([]byte, cacheProcessCountAt+4)
	binary.LittleEndian.PutUint32(data[:4], cacheMagic)
	binary.LittleEndian.PutUint32(data[cacheMajorOffset:], cacheMajorVersion)
	binary.LittleEndian.PutUint32(data[cacheMinorOffset:], cacheMinorVersion)
	binary.LittleEndian.PutUint32(data[cacheProcessCountAt:], 2)
	putProcess := func(process, device int, memory, sm uint64) {
		base := cacheProcessBase + process*cacheProcessSize
		binary.LittleEndian.PutUint32(data[base+cachePIDOffset:], uint32(100+process))
		binary.LittleEndian.PutUint64(data[base+cacheMemoryBase+device*cacheDeviceMemorySize+cacheMemoryTotalOffset:], memory)
		binary.LittleEndian.PutUint64(data[base+cacheDeviceUtilBase+device*cacheDeviceUtilSize+cacheSMUtilOffset:], sm)
	}
	putProcess(0, 0, 3<<30, 25)
	putProcess(1, 0, 2<<30, 30)
	putProcess(0, 1, 1<<30, 15)

	snapshot, err := parseCache(data, 2)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MajorVersion != 1 || snapshot.MinorVersion != 2 {
		t.Fatalf("version = %d.%d", snapshot.MajorVersion, snapshot.MinorVersion)
	}
	if got := snapshot.Devices[0]; got.MemoryUsedBytes != 5<<30 || got.SMUtilPercent != 55 {
		t.Fatalf("device 0 = %+v", got)
	}
	if got := snapshot.Devices[1]; got.MemoryUsedBytes != 1<<30 || got.SMUtilPercent != 15 {
		t.Fatalf("device 1 = %+v", got)
	}
}

func TestParseCacheRejectsUninitializedAndVersionDrift(t *testing.T) {
	data := make([]byte, cacheProcessCountAt+4)
	if _, err := parseCache(data, 1); !errors.Is(err, ErrCacheUninitialized) {
		t.Fatalf("uninitialized error = %v", err)
	}
	binary.LittleEndian.PutUint32(data[:4], cacheMagic)
	binary.LittleEndian.PutUint32(data[cacheMajorOffset:], 2)
	binary.LittleEndian.PutUint32(data[cacheMinorOffset:], 0)
	if _, err := parseCache(data, 1); !errors.Is(err, ErrCacheVersion) {
		t.Fatalf("version error = %v", err)
	}
}

func TestParseCacheSkipsUnstableProcessSlot(t *testing.T) {
	data := make([]byte, cacheProcessCountAt+4)
	binary.LittleEndian.PutUint32(data[:4], cacheMagic)
	binary.LittleEndian.PutUint32(data[cacheMajorOffset:], cacheMajorVersion)
	binary.LittleEndian.PutUint32(data[cacheMinorOffset:], cacheMinorVersion)
	binary.LittleEndian.PutUint32(data[cacheProcessCountAt:], 1)
	binary.LittleEndian.PutUint32(data[cacheProcessBase:], 100)
	binary.LittleEndian.PutUint64(data[cacheProcessBase+cacheSequenceLockOffset:], 1)
	snapshot, err := parseCache(data, 1)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Devices[0].MemoryUsedBytes != 0 {
		t.Fatalf("unstable slot was counted: %+v", snapshot.Devices[0])
	}
}

// A new limit is written for every visible device and only for them; the
// same limit again changes nothing; a device holding more than the limit
// refuses the change and nothing is written.
func TestSetLimitRewritesVisibleDevicesUnlessBelowUsage(t *testing.T) {
	data := make([]byte, cacheProcessCountAt+4)
	binary.LittleEndian.PutUint32(data[:4], cacheMagic)
	binary.LittleEndian.PutUint32(data[cacheMajorOffset:], cacheMajorVersion)
	binary.LittleEndian.PutUint32(data[cacheMinorOffset:], cacheMinorVersion)
	binary.LittleEndian.PutUint32(data[cacheProcessCountAt:], 1)
	binary.LittleEndian.PutUint32(data[cacheProcessBase+cachePIDOffset:], 100)
	binary.LittleEndian.PutUint64(data[cacheProcessBase+cacheMemoryBase+cacheMemoryTotalOffset:], 5<<30)
	limits := func() []uint64 {
		out := make([]uint64, 3)
		for i := range out {
			out[i] = binary.LittleEndian.Uint64(data[cacheLimitOffset+i*8:])
		}
		return out
	}
	for i := 0; i < 3; i++ {
		binary.LittleEndian.PutUint64(data[cacheLimitOffset+i*8:], 6<<30)
	}

	changed, err := setLimit(data, 2, 9<<30)
	if err != nil || !changed {
		t.Fatalf("raise: changed=%v err=%v", changed, err)
	}
	if got := limits(); got[0] != 9<<30 || got[1] != 9<<30 || got[2] != 6<<30 {
		t.Fatalf("limits after raise = %v", got)
	}
	if changed, err := setLimit(data, 2, 9<<30); err != nil || changed {
		t.Fatalf("same limit: changed=%v err=%v", changed, err)
	}
	if _, err := setLimit(data, 2, 4<<30); !errors.Is(err, ErrLimitBelowUsage) {
		t.Fatalf("below usage err = %v", err)
	}
	if got := limits(); got[0] != 9<<30 || got[1] != 9<<30 {
		t.Fatalf("a refused change was written: %v", got)
	}
	if changed, err := setLimit(data, 2, 5<<30); err != nil || !changed {
		t.Fatalf("down to usage: changed=%v err=%v", changed, err)
	}
	if _, err := setLimit(make([]byte, cacheProcessCountAt+4), 1, 8<<30); !errors.Is(err, ErrCacheUninitialized) {
		t.Fatalf("uninitialised cache err = %v", err)
	}
}

// SetMemoryLimit works on the file the instance's container maps.
func TestSetMemoryLimitOnFile(t *testing.T) {
	data := make([]byte, cacheProcessCountAt+4)
	binary.LittleEndian.PutUint32(data[:4], cacheMagic)
	binary.LittleEndian.PutUint32(data[cacheMajorOffset:], cacheMajorVersion)
	binary.LittleEndian.PutUint32(data[cacheMinorOffset:], cacheMinorVersion)
	path := filepath.Join(t.TempDir(), "instance.cache")
	if err := os.WriteFile(path, data, 0o666); err != nil {
		t.Fatal(err)
	}
	if changed, err := SetMemoryLimit(path, 1, 12<<30); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if limit := binary.LittleEndian.Uint64(got[cacheLimitOffset:]); limit != 12<<30 {
		t.Fatalf("limit on disk = %d", limit)
	}
	if _, err := SetMemoryLimit(filepath.Join(t.TempDir(), "missing.cache"), 1, 1<<30); !os.IsNotExist(err) {
		t.Fatalf("missing cache err = %v", err)
	}
	if _, err := SetMemoryLimit(path, 1, 0); err == nil {
		t.Fatal("a zero limit (unlimited to HAMi-core) was accepted")
	}
}
