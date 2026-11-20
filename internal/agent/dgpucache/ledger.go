package dgpucache

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"syscall"
)

// The PPU and Ascend partition shims keep what a slice holds in a region of
// their own, as HAMi-core does in its cache. Each layout below is the one the
// packaged shim is built from (build/runtime-assets/ppu and
// build/runtime-assets/ascend); a region of any other version is refused,
// because reading a shifted layout would fabricate usage. Both shims write in
// the host's byte order and neither asks a reader to take its lock: a figure
// one allocation stale is the documented cost.

// ppu-slicing-shim common/vppu_ledger.h, layout version 1: struct vppu_region
// and its struct vppu_device_usage entries.
const (
	ppuMagic            = "VPPUREGN"
	ppuVersion          = uint32(1)
	ppuVersionOffset    = 8
	ppuHeaderOffset     = 12 // offset of devices[], as the region states it
	ppuDeviceSlotsAt    = 16
	ppuProcessSlotsAt   = 20
	ppuHeaderBytes      = 96
	ppuProcessSlots     = 32
	ppuDeviceBytes      = 64 + ppuProcessSlots*16
	ppuMemoryUsedOffset = 8
	ppuSMUtilOffset     = 20
)

// ReadPPULedger reads a PPU slice's usage region: what the instance's
// processes are charged on each of its cards and, on a card whose compute is
// capped, the utilization the shim's controller last measured. Devices are in
// the order the instance was given its cards. A card the instance has not
// allocated on yet reads zero.
func ReadPPULedger(path string, deviceCount int) ([]DeviceUsage, error) {
	if deviceCount <= 0 {
		return nil, fmt.Errorf("invalid PPU ledger device count %d", deviceCount)
	}
	data, err := readLedger(path)
	if err != nil {
		return nil, err
	}
	// The shim stamps the magic last, so a region without one is still being
	// set up.
	if len(data) < ppuHeaderBytes || allZero(data[:len(ppuMagic)]) {
		return nil, ErrCacheUninitialized
	}
	if string(data[:len(ppuMagic)]) != ppuMagic {
		return nil, fmt.Errorf("%s is not a PPU usage ledger", path)
	}
	order := binary.NativeEndian
	if version := order.Uint32(data[ppuVersionOffset:]); version != ppuVersion {
		return nil, fmt.Errorf("%w: PPU ledger layout %d, want %d", ErrCacheVersion, version, ppuVersion)
	}
	header := int(order.Uint32(data[ppuHeaderOffset:]))
	devices, processes := int(order.Uint32(data[ppuDeviceSlotsAt:])), int(order.Uint32(data[ppuProcessSlotsAt:]))
	if header < ppuHeaderBytes || processes != ppuProcessSlots || deviceCount > devices {
		return nil, fmt.Errorf("%w: PPU ledger with header %d, %d device and %d process slots", ErrCacheVersion, header, devices, processes)
	}
	if header > len(data) || deviceCount > (len(data)-header)/ppuDeviceBytes {
		return nil, fmt.Errorf("PPU ledger too small: %d bytes", len(data))
	}
	out := make([]DeviceUsage, deviceCount)
	for device := range out {
		at := header + device*ppuDeviceBytes
		out[device] = DeviceUsage{
			MemoryUsedBytes: order.Uint64(data[at+ppuMemoryUsedOffset:]),
			SMUtilPercent:   min(uint64(order.Uint32(data[at+ppuSMUtilOffset:])), 100),
		}
	}
	return out, nil
}

// vcann-rt memory_shared_ledger_t (src/memory_ledger.c, added by
// 0002-synchronous-memory-ledger.patch), version 1: a 32-byte header and 64
// process slots of 24 bytes.
const (
	ascendMagic           = uint64(0x454E50554D454D31) // "ENPUMEM1"
	ascendVersion         = uint32(1)
	ascendVersionOffset   = 8
	ascendStructureOffset = 12
	ascendUsedOffset      = 24
	ascendHeaderBytes     = 32
	ascendStructureBytes  = ascendHeaderBytes + 64*24
)

// ReadAscendLedger reads an Ascend slice's memory ledger: what the instance's
// processes are charged against its quota on its one card. The ledger carries
// no compute figure.
func ReadAscendLedger(path string) (DeviceUsage, error) {
	data, err := readLedger(path)
	if err != nil {
		return DeviceUsage{}, err
	}
	// The first process sizes the ledger, clears it and then stamps it.
	if len(data) < ascendHeaderBytes || allZero(data[:8]) {
		return DeviceUsage{}, ErrCacheUninitialized
	}
	order := binary.NativeEndian
	if order.Uint64(data) != ascendMagic {
		return DeviceUsage{}, fmt.Errorf("%s is not an Ascend memory ledger", path)
	}
	version, size := order.Uint32(data[ascendVersionOffset:]), order.Uint32(data[ascendStructureOffset:])
	if version != ascendVersion || size != ascendStructureBytes {
		return DeviceUsage{}, fmt.Errorf("%w: Ascend ledger version %d of %d bytes, want %d of %d", ErrCacheVersion, version, size, ascendVersion, ascendStructureBytes)
	}
	if len(data) < ascendStructureBytes {
		return DeviceUsage{}, fmt.Errorf("Ascend ledger too small: %d bytes", len(data))
	}
	return DeviceUsage{MemoryUsedBytes: order.Uint64(data[ascendUsedOffset:])}, nil
}

// ledgerReadLimit bounds a read: the PPU region is 36960 bytes and the
// Ascend ledger 1568.
const ledgerReadLimit = 1 << 20

// readLedger reads a ledger file, which is small and of a fixed size.
func readLedger(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular ledger file", path)
	}
	return io.ReadAll(io.LimitReader(f, ledgerReadLimit))
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}
