// Package safetensors parses HuggingFace safetensors metadata to compute
// model weight sizes. It uses only the Go standard library.
package safetensors

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// TensorMeta is one tensor's header entry.
type TensorMeta struct {
	Dtype   string  `json:"dtype"`
	Shape   []int64 `json:"shape"`
	Offsets []int64 `json:"data_offsets"` // [begin,end] within the byte buffer
}

// Info aggregates weight sizes.
type Info struct {
	TotalBytes int64            // total weight bytes
	PerDtype   map[string]int64 // bytes per dtype (e.g. "F16","BF16","F8_E4M3")
	NumTensors int
}

// maxHeaderLen bounds the JSON header size we are willing to read, guarding
// against a corrupt or hostile length prefix causing a huge allocation.
const maxHeaderLen = 100 << 20 // 100 MiB

// ParseIndexJSON parses model.safetensors.index.json. It returns TotalBytes from
// metadata.total_size (the fast path). PerDtype is nil (index.json has no dtypes).
func ParseIndexJSON(r io.Reader) (Info, error) {
	var doc struct {
		Metadata struct {
			TotalSize json.Number `json:"total_size"`
		} `json:"metadata"`
		WeightMap map[string]string `json:"weight_map"`
	}
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return Info{}, fmt.Errorf("safetensors: decode index.json: %w", err)
	}
	var total int64
	if s := doc.Metadata.TotalSize.String(); s != "" {
		n, err := doc.Metadata.TotalSize.Int64()
		if err != nil {
			return Info{}, fmt.Errorf("safetensors: total_size %q: %w", s, err)
		}
		total = n
	}
	return Info{
		TotalBytes: total,
		PerDtype:   nil,
		NumTensors: len(doc.WeightMap),
	}, nil
}

// ParseHeader parses a single .safetensors file's header: an 8-byte little-endian
// uint64 header length, followed by that many bytes of JSON mapping tensor name ->
// TensorMeta (plus an optional "__metadata__" key which must be skipped). It
// returns the per-tensor map. Only reads the header, not the tensor data.
func ParseHeader(r io.Reader) (map[string]TensorMeta, error) {
	var lenBuf [8]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("safetensors: read header length: %w", err)
	}
	hlen := binary.LittleEndian.Uint64(lenBuf[:])
	if hlen > maxHeaderLen {
		return nil, fmt.Errorf("safetensors: header length %d exceeds max %d", hlen, maxHeaderLen)
	}
	jsonBuf := make([]byte, hlen)
	if _, err := io.ReadFull(r, jsonBuf); err != nil {
		return nil, fmt.Errorf("safetensors: read header json: %w", err)
	}

	// Decode into raw messages first so we can skip the "__metadata__" key,
	// whose value is an arbitrary object rather than a TensorMeta.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(jsonBuf, &raw); err != nil {
		return nil, fmt.Errorf("safetensors: unmarshal header: %w", err)
	}

	out := make(map[string]TensorMeta, len(raw))
	for name, rm := range raw {
		if name == "__metadata__" {
			continue
		}
		var tm TensorMeta
		if err := json.Unmarshal(rm, &tm); err != nil {
			return nil, fmt.Errorf("safetensors: unmarshal tensor %q: %w", name, err)
		}
		out[name] = tm
	}
	return out, nil
}

// InfoFromHeader aggregates a parsed header into an Info (TotalBytes + PerDtype),
// computing each tensor's bytes as offsets[1]-offsets[0].
func InfoFromHeader(h map[string]TensorMeta) Info {
	info := Info{
		PerDtype:   make(map[string]int64, len(h)),
		NumTensors: len(h),
	}
	for _, tm := range h {
		var n int64
		if len(tm.Offsets) >= 2 {
			n = tm.Offsets[1] - tm.Offsets[0]
		}
		info.TotalBytes += n
		info.PerDtype[tm.Dtype] += n
	}
	return info
}

// DtypeSize returns the byte size of a safetensors dtype string
// (F64/I64->8, F32/I32->4, F16/BF16/I16->2, F8_E4M3/F8_E5M2/I8/U8->1, F4->0 packed).
// Unknown returns 0.
func DtypeSize(dtype string) int {
	switch dtype {
	case "F64", "I64", "U64":
		return 8
	case "F32", "I32", "U32":
		return 4
	case "F16", "BF16", "I16", "U16":
		return 2
	case "F8_E4M3", "F8_E5M2", "I8", "U8", "BOOL":
		return 1
	case "F4":
		return 0 // packed, sub-byte
	default:
		return 0
	}
}
