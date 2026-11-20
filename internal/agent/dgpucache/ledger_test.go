package dgpucache

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The fixtures are written by testdata/ledger_fixture.c from the shims' own
// struct definitions, so these tests read what the packaged libraries write
// rather than bytes laid out by the reader's own offsets.

const mib = uint64(1) << 20

func ledgerFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeLedger(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage-ledger")
	if err := os.WriteFile(path, data, 0o666); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadPPULedgerReadsEachCardOfTheSlice(t *testing.T) {
	path := writeLedger(t, ledgerFixture(t, "ppu-ledger-v1.bin"))
	devices, err := ReadPPULedger(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []DeviceUsage{
		{MemoryUsedBytes: 7168 * mib, SMUtilPercent: 37},
		{MemoryUsedBytes: 2048*mib + 4096},
	}
	if len(devices) != 2 || devices[0] != want[0] || devices[1] != want[1] {
		t.Fatalf("devices = %+v, want %+v", devices, want)
	}
	// A one-card instance reads only its card.
	if devices, err = ReadPPULedger(path, 1); err != nil || len(devices) != 1 || devices[0] != want[0] {
		t.Fatalf("one card = %+v, %v", devices, err)
	}
	// A card the instance holds and has not allocated on reads zero.
	if devices, err = ReadPPULedger(path, 3); err != nil || devices[2] != (DeviceUsage{}) {
		t.Fatalf("untouched card = %+v, %v", devices, err)
	}
}

func TestReadPPULedgerRefusesWhatItCannotParse(t *testing.T) {
	valid := ledgerFixture(t, "ppu-ledger-v1.bin")
	mutate := func(change func(data []byte)) []byte {
		data := append([]byte(nil), valid...)
		change(data)
		return data
	}
	for _, tc := range []struct {
		name    string
		data    []byte
		devices int
		is      error // nil: any other error
	}{
		{"placeholder the agent created", nil, 1, ErrCacheUninitialized},
		{"sized but not stamped yet", make([]byte, len(valid)), 1, ErrCacheUninitialized},
		{"another file", mutate(func(data []byte) { copy(data, "NOTAVPPU") }), 1, nil},
		{"newer layout", mutate(func(data []byte) { binary.NativeEndian.PutUint32(data[8:], 2) }), 1, ErrCacheVersion},
		{"other process table", mutate(func(data []byte) { binary.NativeEndian.PutUint32(data[20:], 64) }), 1, ErrCacheVersion},
		{"more cards than slots", mutate(func(data []byte) { binary.NativeEndian.PutUint32(data[16:], 1) }), 2, ErrCacheVersion},
		{"truncated", valid[:96+576-1], 1, nil},
		{"no cards asked", valid, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			devices, err := ReadPPULedger(writeLedger(t, tc.data), tc.devices)
			if err == nil || devices != nil {
				t.Fatalf("read = %+v, %v; want an error and no figures", devices, err)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("error = %v, want %v", err, tc.is)
			}
			if tc.is == nil && (errors.Is(err, ErrCacheUninitialized) || errors.Is(err, ErrCacheVersion)) {
				t.Fatalf("error = %v, want one that is logged", err)
			}
		})
	}
	if _, err := ReadPPULedger(filepath.Join(t.TempDir(), "absent"), 1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent ledger error = %v", err)
	}
}

func TestReadPPULedgerCapsComputeAtTheWholeCard(t *testing.T) {
	data := ledgerFixture(t, "ppu-ledger-v1.bin")
	binary.NativeEndian.PutUint32(data[96+20:], 180)
	devices, err := ReadPPULedger(writeLedger(t, data), 1)
	if err != nil || devices[0].SMUtilPercent != 100 {
		t.Fatalf("devices = %+v, %v", devices, err)
	}
}

func TestReadAscendLedgerReadsTheSliceTotal(t *testing.T) {
	device, err := ReadAscendLedger(writeLedger(t, ledgerFixture(t, "ascend-ledger-v1.bin")))
	if err != nil {
		t.Fatal(err)
	}
	if device != (DeviceUsage{MemoryUsedBytes: 6860 * mib}) {
		t.Fatalf("device = %+v", device)
	}
}

func TestReadAscendLedgerRefusesWhatItCannotParse(t *testing.T) {
	valid := ledgerFixture(t, "ascend-ledger-v1.bin")
	mutate := func(change func(data []byte)) []byte {
		data := append([]byte(nil), valid...)
		change(data)
		return data
	}
	for _, tc := range []struct {
		name string
		data []byte
		is   error
	}{
		{"placeholder the agent created", nil, ErrCacheUninitialized},
		{"sized and cleared, not stamped yet", make([]byte, len(valid)), ErrCacheUninitialized},
		{"another file", mutate(func(data []byte) { copy(data, "NOTALEDGER") }), nil},
		{"newer version", mutate(func(data []byte) { binary.NativeEndian.PutUint32(data[8:], 2) }), ErrCacheVersion},
		{"other structure size", mutate(func(data []byte) { binary.NativeEndian.PutUint32(data[12:], 3104) }), ErrCacheVersion},
		{"truncated process table", valid[:len(valid)-1], nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device, err := ReadAscendLedger(writeLedger(t, tc.data))
			if err == nil || device != (DeviceUsage{}) {
				t.Fatalf("read = %+v, %v; want an error and no figure", device, err)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("error = %v, want %v", err, tc.is)
			}
			if tc.is == nil && (errors.Is(err, ErrCacheUninitialized) || errors.Is(err, ErrCacheVersion)) {
				t.Fatalf("error = %v, want one that is logged", err)
			}
		})
	}
	if _, err := ReadAscendLedger(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent ledger error = %v", err)
	}
}

func TestReadAscendLedgerRefusesNonRegularSharedMemoryObjects(t *testing.T) {
	dir := t.TempDir()
	pipe := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(writeLedger(t, ledgerFixture(t, "ascend-ledger-v1.bin")), link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, pipe, link} {
		if _, err := ReadAscendLedger(path); err == nil {
			t.Errorf("accepted non-regular shared memory object %q", path)
		}
	}
}
