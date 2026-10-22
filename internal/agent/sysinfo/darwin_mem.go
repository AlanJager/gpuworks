package sysinfo

// darwinMem turns macOS's memory facts into the total and available kB the
// Linux collector reads from /proc/meminfo. kern.memorystatus_level is the
// percentage of memory the kernel considers free for new work - the figure
// memory_pressure prints as "System-wide memory free percentage" - which, like
// MemAvailable, counts reclaimable pages rather than only idle ones. It is in
// a file without a build tag so the arithmetic is tested on every platform.
func darwinMem(memBytes uint64, freePercent uint32, haveLevel bool) (totalKB, availKB int64) {
	totalKB = int64(memBytes / 1024)
	if !haveLevel || freePercent > 100 {
		return totalKB, 0
	}
	return totalKB, totalKB * int64(freePercent) / 100
}

// statfsKB converts statfs block counts to kB.
func statfsKB(blockSize, blocks, availBlocks uint64) (totalKB, availKB int64) {
	return int64(blocks * blockSize / 1024), int64(availBlocks * blockSize / 1024)
}
