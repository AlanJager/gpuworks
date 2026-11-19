package storagemount

import "math"

// BlockBytes is blocks of size bytes each, in bytes, saturating at the
// largest int64: a network filesystem may report a size whose product does
// not fit (an Alibaba Cloud NAS answers statfs with close to 2^63 bytes), and
// a wrapped product would read as a negative capacity.
func BlockBytes(blocks, size uint64) int64 {
	if size == 0 || blocks == 0 {
		return 0
	}
	if blocks > math.MaxInt64/size {
		return math.MaxInt64
	}
	return int64(blocks * size)
}
