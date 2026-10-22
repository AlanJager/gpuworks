package safetensors

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// makeHeader builds a valid safetensors byte buffer: an 8-byte little-endian
// header length followed by the JSON header body.
func makeHeader(jsonBody string) []byte {
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(jsonBody)))
	return append(lenBuf[:], []byte(jsonBody)...)
}

func TestParseIndexJSON(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantTotal int64
		wantNum   int
		wantErr   bool
	}{
		{
			name:      "basic",
			input:     `{"metadata":{"total_size":16060522496},"weight_map":{"a":"f1","b":"f1"}}`,
			wantTotal: 16060522496,
			wantNum:   2,
		},
		{
			name:      "large total_size",
			input:     `{"metadata":{"total_size":9007199254740993},"weight_map":{}}`,
			wantTotal: 9007199254740993,
			wantNum:   0,
		},
		{
			name:      "missing metadata",
			input:     `{"weight_map":{"a":"f1"}}`,
			wantTotal: 0,
			wantNum:   1,
		},
		{
			name:    "invalid json",
			input:   `{not json`,
			wantErr: true,
		},
		{
			name:    "total_size not integer",
			input:   `{"metadata":{"total_size":"big"}}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ParseIndexJSON(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if info.TotalBytes != tt.wantTotal {
				t.Errorf("TotalBytes = %d, want %d", info.TotalBytes, tt.wantTotal)
			}
			if info.NumTensors != tt.wantNum {
				t.Errorf("NumTensors = %d, want %d", info.NumTensors, tt.wantNum)
			}
			if info.PerDtype != nil {
				t.Errorf("PerDtype = %v, want nil", info.PerDtype)
			}
		})
	}
}

func TestParseHeader(t *testing.T) {
	t.Run("with __metadata__ present", func(t *testing.T) {
		body := `{"__metadata__":{"format":"pt"},` +
			`"model.embed_tokens.weight":{"dtype":"BF16","shape":[152064,3584],"data_offsets":[0,1089798144]},` +
			`"model.norm.weight":{"dtype":"F32","shape":[3584],"data_offsets":[1089798144,1089812480]}}`
		h, err := ParseHeader(bytes.NewReader(makeHeader(body)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := h["__metadata__"]; ok {
			t.Errorf("__metadata__ key should be skipped")
		}
		if len(h) != 2 {
			t.Fatalf("got %d tensors, want 2", len(h))
		}
		emb := h["model.embed_tokens.weight"]
		if emb.Dtype != "BF16" {
			t.Errorf("dtype = %q, want BF16", emb.Dtype)
		}
		if len(emb.Shape) != 2 || emb.Shape[0] != 152064 || emb.Shape[1] != 3584 {
			t.Errorf("shape = %v, want [152064 3584]", emb.Shape)
		}
		if len(emb.Offsets) != 2 || emb.Offsets[0] != 0 || emb.Offsets[1] != 1089798144 {
			t.Errorf("offsets = %v, want [0 1089798144]", emb.Offsets)
		}
	})

	t.Run("no __metadata__", func(t *testing.T) {
		body := `{"w":{"dtype":"F16","shape":[10],"data_offsets":[0,20]}}`
		h, err := ParseHeader(bytes.NewReader(makeHeader(body)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(h) != 1 {
			t.Fatalf("got %d tensors, want 1", len(h))
		}
	})

	t.Run("truncated length prefix", func(t *testing.T) {
		_, err := ParseHeader(bytes.NewReader([]byte{1, 2, 3}))
		if err == nil {
			t.Fatalf("expected error for short length prefix")
		}
	})

	t.Run("truncated body", func(t *testing.T) {
		var lenBuf [8]byte
		binary.LittleEndian.PutUint64(lenBuf[:], 100)
		buf := append(lenBuf[:], []byte(`{"w":`)...)
		_, err := ParseHeader(bytes.NewReader(buf))
		if err == nil {
			t.Fatalf("expected error for truncated body")
		}
	})

	t.Run("invalid json body", func(t *testing.T) {
		_, err := ParseHeader(bytes.NewReader(makeHeader(`{not json`)))
		if err == nil {
			t.Fatalf("expected error for invalid json")
		}
	})

	t.Run("header too large", func(t *testing.T) {
		var lenBuf [8]byte
		binary.LittleEndian.PutUint64(lenBuf[:], maxHeaderLen+1)
		_, err := ParseHeader(bytes.NewReader(lenBuf[:]))
		if err == nil {
			t.Fatalf("expected error for oversized header")
		}
	})
}

func TestInfoFromHeader(t *testing.T) {
	h := map[string]TensorMeta{
		"a": {Dtype: "BF16", Offsets: []int64{0, 100}},
		"b": {Dtype: "BF16", Offsets: []int64{100, 250}},
		"c": {Dtype: "F32", Offsets: []int64{250, 300}},
		"d": {Dtype: "F8_E4M3", Offsets: []int64{300, 310}},
		"e": {Dtype: "F32"}, // no offsets -> contributes 0
	}
	info := InfoFromHeader(h)

	if info.NumTensors != 5 {
		t.Errorf("NumTensors = %d, want 5", info.NumTensors)
	}
	wantTotal := int64(100 + 150 + 50 + 10 + 0)
	if info.TotalBytes != wantTotal {
		t.Errorf("TotalBytes = %d, want %d", info.TotalBytes, wantTotal)
	}
	wantPer := map[string]int64{
		"BF16":    250,
		"F32":     50,
		"F8_E4M3": 10,
	}
	for dt, want := range wantPer {
		if got := info.PerDtype[dt]; got != want {
			t.Errorf("PerDtype[%q] = %d, want %d", dt, got, want)
		}
	}
	if len(info.PerDtype) != len(wantPer) {
		t.Errorf("PerDtype has %d keys, want %d: %v", len(info.PerDtype), len(wantPer), info.PerDtype)
	}
}

func TestInfoFromHeaderEmpty(t *testing.T) {
	info := InfoFromHeader(map[string]TensorMeta{})
	if info.TotalBytes != 0 || info.NumTensors != 0 {
		t.Errorf("empty header: got %+v", info)
	}
	if info.PerDtype == nil {
		t.Errorf("PerDtype should be non-nil for empty header")
	}
}

func TestDtypeSize(t *testing.T) {
	tests := []struct {
		dtype string
		want  int
	}{
		{"F64", 8}, {"I64", 8}, {"U64", 8},
		{"F32", 4}, {"I32", 4}, {"U32", 4},
		{"F16", 2}, {"BF16", 2}, {"I16", 2}, {"U16", 2},
		{"F8_E4M3", 1}, {"F8_E5M2", 1}, {"I8", 1}, {"U8", 1}, {"BOOL", 1},
		{"F4", 0},
		{"UNKNOWN", 0},
		{"", 0},
	}
	for _, tt := range tests {
		t.Run(tt.dtype, func(t *testing.T) {
			if got := DtypeSize(tt.dtype); got != tt.want {
				t.Errorf("DtypeSize(%q) = %d, want %d", tt.dtype, got, tt.want)
			}
		})
	}
}

// TestEndToEnd exercises ParseHeader -> InfoFromHeader on a realistic buffer.
func TestEndToEnd(t *testing.T) {
	body := `{"__metadata__":{"format":"pt"},` +
		`"t1":{"dtype":"BF16","shape":[2,4],"data_offsets":[0,16]},` +
		`"t2":{"dtype":"BF16","shape":[4],"data_offsets":[16,24]},` +
		`"t3":{"dtype":"F8_E4M3","shape":[8],"data_offsets":[24,32]}}`
	h, err := ParseHeader(bytes.NewReader(makeHeader(body)))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	info := InfoFromHeader(h)
	if info.TotalBytes != 32 {
		t.Errorf("TotalBytes = %d, want 32", info.TotalBytes)
	}
	if info.PerDtype["BF16"] != 24 {
		t.Errorf("PerDtype[BF16] = %d, want 24", info.PerDtype["BF16"])
	}
	if info.PerDtype["F8_E4M3"] != 8 {
		t.Errorf("PerDtype[F8_E4M3] = %d, want 8", info.PerDtype["F8_E4M3"])
	}
	if info.NumTensors != 3 {
		t.Errorf("NumTensors = %d, want 3", info.NumTensors)
	}
}
