package modelmetadata

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
)

// Index size scales with tensor count, unlike config.json. Bound its I/O
// separately and retain only unique shard paths while decoding the JSON.
const (
	maxCheckpointIndexBytes    = 128 << 20
	checkpointIndexBufferBytes = 256 << 10
)

func readCheckpointIndex(ctx context.Context, files FileView, name string) ([]string, bool, error) {
	info, err := files.Stat(ctx, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.IsDir {
		return nil, true, fs.ErrInvalid
	}
	if info.Size > maxCheckpointIndexBytes {
		return nil, true, ErrReadLimit
	}
	reader := &fileRangeReader{ctx: ctx, files: files, name: name, size: info.Size}
	dec := json.NewDecoder(bufio.NewReaderSize(reader, checkpointIndexBufferBytes))
	dec.UseNumber()
	if err := expectJSONDelimiter(dec, '{'); err != nil {
		return nil, true, err
	}
	shards := map[string]bool{}
	seenMap := false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, true, err
		}
		if key != "weight_map" {
			if err := skipJSONValue(dec); err != nil {
				return nil, true, err
			}
			continue
		}
		if seenMap {
			return nil, true, errors.New("duplicate weight_map")
		}
		seenMap = true
		if err := expectJSONDelimiter(dec, '{'); err != nil {
			return nil, true, err
		}
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return nil, true, err
			}
			var target string
			if err := dec.Decode(&target); err != nil {
				return nil, true, err
			}
			shards[target] = true
		}
		if err := expectJSONDelimiter(dec, '}'); err != nil {
			return nil, true, err
		}
	}
	if err := expectJSONDelimiter(dec, '}'); err != nil {
		return nil, true, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("data after checkpoint index")
		}
		return nil, true, err
	}
	paths := make([]string, 0, len(shards))
	for path := range shards {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths, true, nil
}

func expectJSONDelimiter(dec *json.Decoder, want json.Delim) error {
	got, err := dec.Token()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("checkpoint index: expected %q", want)
	}
	return nil
}

func skipJSONValue(dec *json.Decoder) error {
	depth := 0
	for {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// fileRangeReader adapts bounded range reads to a streaming JSON decoder.
type fileRangeReader struct {
	ctx          context.Context
	files        FileView
	name         string
	size, offset int64
}

func (r *fileRangeReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.offset == r.size {
		return 0, io.EOF
	}
	length := min(int64(len(p)), r.size-r.offset, checkpointIndexBufferBytes)
	b, err := r.files.ReadRange(r.ctx, r.name, r.offset, length)
	n := copy(p, b)
	r.offset += int64(n)
	if err == nil && int64(n) < length {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
