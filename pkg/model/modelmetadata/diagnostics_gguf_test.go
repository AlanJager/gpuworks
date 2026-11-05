package modelmetadata

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"testing"
)

func diagnosticGGUFValue(key string, kind uint32, value []byte) string {
	b := []byte(gguf("", "", 0, 0))
	binary.LittleEndian.PutUint64(b[16:24], 1)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(key)))
	b = append(b, key...)
	b = binary.LittleEndian.AppendUint32(b, kind)
	return string(append(b, value...))
}

func TestDiagnosticGGUFHeaderRejections(t *testing.T) {
	base := []byte(gguf("llama", "model", 0, 0))
	mutate := func(offset int, value uint64, width int) string {
		b := append([]byte(nil), base...)
		if width == 4 {
			binary.LittleEndian.PutUint32(b[offset:], uint32(value))
		} else {
			binary.LittleEndian.PutUint64(b[offset:], value)
		}
		return string(b)
	}
	array := func(kind uint32, count uint64, data []byte) []byte {
		b := binary.LittleEndian.AppendUint32(nil, kind)
		return append(binary.LittleEndian.AppendUint64(b, count), data...)
	}
	for _, tc := range []struct{ name, body, cause string }{
		{"magic", "NOPE" + string(base[4:]), "invalid GGUF magic"},
		{"version", mutate(4, 1, 4), "unsupported GGUF version"},
		{"no tensors", mutate(8, 0, 8), "no tensors"},
		{"too many metadata keys", mutate(16, 4097, 8), "read limit"},
		{"oversized metadata key", mutate(24, maxDocumentBytes+1, 8), "read limit"},
		{"architecture type", diagnosticGGUFValue("general.architecture", 4, nil), "identity must be a string"},
		{"model type", diagnosticGGUFValue("general.type", 4, nil), "identity must be a string"},
		{"signed split", diagnosticGGUFValue("split.no", 3, nil), "split value must be unsigned"},
		{"unknown metadata type", diagnosticGGUFValue("custom", 100, nil), "unknown GGUF metadata type"},
		{"truncated scalar", diagnosticGGUFValue("custom", 10, nil), "unexpected EOF"},
		{"oversized string", diagnosticGGUFValue("general.architecture", 8, binary.LittleEndian.AppendUint64(nil, maxDocumentBytes+1)), "read limit"},
		{"truncated skipped string", diagnosticGGUFValue("custom", 8, binary.LittleEndian.AppendUint64(nil, 8)), "unexpected EOF"},
		{"oversized array", diagnosticGGUFValue("custom", 9, array(0, (1<<20)+1, nil)), "read limit"},
		{"invalid array element", diagnosticGGUFValue("custom", 9, array(9, 1, nil)), "invalid GGUF array element type"},
		{"truncated scalar array", diagnosticGGUFValue("custom", 9, array(10, 2, nil)), "unexpected EOF"},
		{"truncated string array", diagnosticGGUFValue("custom", 9, array(8, 1, binary.LittleEndian.AppendUint64(nil, 4))), "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse(context.Background(), Input{}, memory(map[string]string{"model.gguf": tc.body}))
			requireDiagnostic(t, r, err, "model.gguf", tc.cause)
		})
	}
}

// Every truncation point must reject the header without a panic or losing its
// file path. Include all variable-width fields and skipped array encodings.
func TestDiagnosticGGUFTruncatedHeaders(t *testing.T) {
	array := binary.LittleEndian.AppendUint32(nil, 8)
	array = binary.LittleEndian.AppendUint64(array, 1)
	array = binary.LittleEndian.AppendUint64(array, 4)
	array = append(array, "word"...)
	for _, body := range []string{
		gguf("llama", "model", 0, 2),
		diagnosticGGUFValue("split.no", 4, binary.LittleEndian.AppendUint32(nil, 0)),
		diagnosticGGUFValue("split.count", 10, binary.LittleEndian.AppendUint64(nil, 1)),
		diagnosticGGUFValue("tokens", 9, array),
	} {
		for i := 0; i < len(body); i++ {
			r, err := Parse(context.Background(), Input{}, memory(map[string]string{"model.gguf": body[:i]}))
			requireDiagnostic(t, r, err, "model.gguf", "unexpected EOF")
		}
	}
}

func TestDiagnosticGGUFSplitRejections(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		cause string
	}{
		{"too many parts", map[string]string{"model-00001-of-10001.gguf": gguf("llama", "model", 0, 2)}, "invalid split count 10001"},
		{"wrong part number", map[string]string{"model-00001-of-00001.gguf": gguf("llama", "model", 1, 1)}, "split.no=1"},
		{"wrong part count", map[string]string{"model-00001-of-00001.gguf": gguf("llama", "model", 0, 2)}, "split.count=2"},
		{"no split metadata", map[string]string{"model-00001-of-00001.gguf": gguf("llama", "model", 0, 0)}, "split metadata"},
		{"missing first part", map[string]string{"model-00002-of-00002.gguf": gguf("", "", 1, 2)}, "model-00001-of-00002.gguf"},
		{"missing last part", map[string]string{"model-00001-of-00002.gguf": gguf("llama", "model", 0, 2)}, "model-00002-of-00002.gguf"},
		{"unreadable sibling", map[string]string{"model-00001-of-00002.gguf": gguf("llama", "model", 0, 2), "model-00002-of-00002.gguf": "broken"}, "invalid GGUF magic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse(context.Background(), Input{}, memory(tc.files))
			requireDiagnostic(t, r, err, tc.cause)
		})
	}
	for _, identity := range [][2]string{{"clip", ""}, {"controlvector", ""}, {"llama", "adapter"}} {
		r, err := Parse(context.Background(), Input{}, memory(map[string]string{"component.gguf": gguf(identity[0], identity[1], 0, 0)}))
		requireDiagnostic(t, r, err, "component.gguf", "component")
	}
}

func TestDiagnosticGGUFReadFailures(t *testing.T) {
	for _, mode := range []string{"stat", "read"} {
		for _, failure := range []error{fs.ErrPermission, io.ErrUnexpectedEOF} {
			t.Run(mode+"/"+failure.Error(), func(t *testing.T) {
				v := diagnosticFaultView{FileView: memory(map[string]string{"model.gguf": gguf("llama", "model", 0, 0)})}
				if mode == "stat" {
					v.stats = map[string]error{"model.gguf": failure}
				} else {
					v.reads = map[string]error{"model.gguf": failure}
				}
				r, err := Parse(context.Background(), Input{}, v)
				requireDiagnostic(t, r, err, "model.gguf", failure.Error())
			})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	v := diagnosticCancelReadView{FileView: memory(map[string]string{"model.gguf": gguf("llama", "model", 0, 0)}), cancel: cancel}
	_, err := Parse(ctx, Input{}, v)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost GGUF cancellation: %v", err)
	}
}

type diagnosticCancelReadView struct {
	FileView
	cancel context.CancelFunc
}

func (v diagnosticCancelReadView) ReadRange(context.Context, string, int64, int64) ([]byte, error) {
	v.cancel()
	return nil, context.Canceled
}

func TestDiagnosticGGUFReadBudgets(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    ggufReader
		n    int64
		want error
	}{
		{"negative length", ggufReader{size: 10}, -1, io.ErrUnexpectedEOF},
		{"oversized range", ggufReader{size: 3 << 20}, maxDocumentBytes + 1, io.ErrUnexpectedEOF},
		{"metadata span", ggufReader{size: maxGGUFMetadataSpan + 1, offset: maxGGUFMetadataSpan}, 1, ErrReadLimit},
		{"read budget", ggufReader{size: 1, readBytes: maxGGUFReadBytes}, 1, ErrReadLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.r.ctx = context.Background()
			_, err := tc.r.take(tc.n)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v got %v", tc.want, err)
			}
		})
	}
	for _, n := range []uint64{maxGGUFMetadataSpan, maxGGUFMetadataSpan + 1} {
		r := ggufReader{size: 2 * maxGGUFMetadataSpan, offset: 1}
		if err := r.skip(n); !errors.Is(err, ErrReadLimit) {
			t.Fatalf("skip %d: %v", n, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := ggufReader{ctx: ctx}
	if _, err := r.take(1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
