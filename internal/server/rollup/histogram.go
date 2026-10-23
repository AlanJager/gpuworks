package rollup

import (
	"encoding/binary"
	"errors"
	"math"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// histogramBins covers quarter-octave latency bins (uxrecord.LatencyBin) up
// to about 4.6 hours; anything longer lands in the last bin.
const histogramBins = 96

// Histogram counts durations in the request view's quarter-octave latency
// bins, so a percentile read back is within about 10% and agrees with the
// latency breakdown of the monitor.
type Histogram [histogramBins]uint32

// Observe counts one duration in milliseconds.
func (h *Histogram) Observe(ms float64) { h.ObserveN(ms, 1) }

// ObserveN counts n durations of ms milliseconds.
func (h *Histogram) ObserveN(ms float64, n int64) {
	bin := uxrecord.LatencyBin(ms)
	if bin >= histogramBins {
		bin = histogramBins - 1
	}
	if bin < 0 {
		bin = 0
	}
	if sum := int64(h[bin]) + n; sum >= math.MaxUint32 {
		h[bin] = math.MaxUint32
	} else {
		h[bin] = uint32(sum)
	}
}

// Add adds o to h.
func (h *Histogram) Add(o Histogram) {
	for i, n := range o {
		if n == 0 {
			continue
		}
		if sum := uint64(h[i]) + uint64(n); sum > math.MaxUint32 {
			h[i] = math.MaxUint32
		} else {
			h[i] = uint32(sum)
		}
	}
}

// Count is how many durations h holds.
func (h *Histogram) Count() uint64 {
	var n uint64
	for _, c := range h {
		n += uint64(c)
	}
	return n
}

// Quantile is the duration below which a fraction q of the counted ones fall
// (the representative of its bin), and false when h is empty.
func (h *Histogram) Quantile(q float64) (float64, bool) {
	total := h.Count()
	if total == 0 {
		return 0, false
	}
	rank := uint64(math.Ceil(q * float64(total)))
	if rank < 1 {
		rank = 1
	}
	var seen uint64
	for bin, c := range h {
		seen += uint64(c)
		if seen >= rank {
			return uxrecord.LatencyBinMS(bin), true
		}
	}
	return uxrecord.LatencyBinMS(histogramBins - 1), true
}

// Encode writes h as (bin, count) pairs of its non-empty bins: a byte and a
// uvarint each, a few dozen bytes for a typical hour. The encoding is the
// stored form of the histogram columns; Decode reads it back.
func (h *Histogram) Encode() []byte {
	out := make([]byte, 0, 32)
	for bin, c := range h {
		if c == 0 {
			continue
		}
		out = append(out, byte(bin))
		out = binary.AppendUvarint(out, uint64(c))
	}
	return out
}

var errHistogram = errors.New("rollup: malformed histogram")

// DecodeHistogram reads an encoded histogram; an empty input is an empty
// histogram.
func DecodeHistogram(b []byte) (Histogram, error) {
	var h Histogram
	for len(b) > 0 {
		bin := int(b[0])
		c, n := binary.Uvarint(b[1:])
		if bin >= histogramBins || n <= 0 || c > math.MaxUint32 {
			return Histogram{}, errHistogram
		}
		h[bin] = uint32(c)
		b = b[1+n:]
	}
	return h, nil
}
