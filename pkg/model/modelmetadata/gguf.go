package modelmetadata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var splitGGUFName = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

type ggufMetadata struct {
	architecture, kind  string
	splitNo, splitCount uint64
	hasSplit            bool
	tensors             uint64
}

func parseGGUF(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	items, err := files.ReadDir(ctx, in.Path)
	if err != nil {
		return RuleResult{}, err
	}
	r := RuleResult{}
	groups := map[string][]string{}
	metadata := map[string]ggufMetadata{}
	for _, item := range items {
		if item.IsDir || !strings.HasSuffix(strings.ToLower(item.Name), ".gguf") {
			continue
		}
		name := path.Join(in.Path, item.Name)
		info, statErr := files.Stat(ctx, name)
		if statErr != nil {
			r.Issues = append(r.Issues, issue(name, "metadata_unreadable", statErr))
			continue
		}
		m, err := readGGUF(ctx, files, name, info.Size)
		if err != nil {
			if ctx.Err() != nil {
				return r, ctx.Err()
			}
			r.Issues = append(r.Issues, issue(name, "invalid_gguf", err))
			continue
		}
		if m.kind == "mmproj" || m.architecture == "clip" || m.architecture == "controlvector" || (m.kind != "" && m.kind != "model") {
			r.Components = append(r.Components, Reference{Path: name, Role: "component", Source: "gguf metadata"})
			continue
		}
		metadata[name] = m
		group := name
		if match := splitGGUFName.FindStringSubmatch(item.Name); match != nil {
			group = path.Join(in.Path, match[1]+"-of-"+match[3])
		}
		groups[group] = append(groups[group], name)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		names := groups[key]
		slices.Sort(names)
		e := Entry{Path: in.Path, Format: "gguf", Files: names}
		lead := metadata[names[0]]
		if match := splitGGUFName.FindStringSubmatch(path.Base(names[0])); match != nil {
			count, _ := strconv.Atoi(match[3])
			if count < 1 || count > 10000 {
				r.Issues = append(r.Issues, issue(names[0], "invalid_gguf_split", nil))
				continue
			}
			for i := 1; i <= count; i++ {
				want := path.Join(in.Path, fmt.Sprintf("%s-%05d-of-%05d.gguf", match[1], i, count))
				m, ok := metadata[want]
				if !ok {
					e.Incomplete = true
					// An existing shard whose header could not be parsed is
					// unresolved, not missing. Its issue preserves the cause.
					if exists(ctx, files, want) {
						e.Files = append(e.Files, want)
					} else {
						e.Missing = append(e.Missing, want)
					}
					continue
				}
				if !m.hasSplit || m.splitNo != uint64(i-1) || m.splitCount != uint64(count) {
					e.Incomplete = true
					r.Issues = append(r.Issues, issue(want, "invalid_gguf_split", nil))
				}
			}
			if !strings.Contains(path.Base(names[0]), "-00001-of-") {
				e.Incomplete = true
			}
		} else if lead.splitCount > 1 {
			e.Incomplete = true
			r.Issues = append(r.Issues, issue(names[0], "unresolved_gguf_split", nil))
		}
		if lead.architecture == "" && !e.Incomplete {
			r.Issues = append(r.Issues, issue(names[0], "gguf_missing_architecture", nil))
			continue
		}
		r.Entries = append(r.Entries, e)
	}
	if len(r.Entries) > 0 {
		for _, ref := range in.Context.Components {
			if strings.HasSuffix(strings.ToLower(ref.Path), ".gguf") && !strings.HasPrefix(ref.Path, in.Path+"/") {
				r.Issues = append(r.Issues, Issue{Path: in.Path, Code: "unresolved_component_association", Detail: ref.Path})
			}
		}
	}
	return r, nil
}

// Metadata reads are buffered and bounded independently of tensor sizes.
// Skipping token arrays advances offsets without downloading tensor payloads.
type ggufReader struct {
	ctx                 context.Context
	files               FileView
	name                string
	size, offset, start int64
	buf                 []byte
	readBytes           int64
}

// Tokenizer arrays alone can exceed 8 MiB. These are metadata parsing budgets,
// independent of tensor payload size and of small JSON document limits.
const (
	maxGGUFReadBytes    = 64 << 20
	maxGGUFMetadataSpan = 128 << 20
)

func (r *ggufReader) take(n int64) ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if n < 0 || n > maxDocumentBytes || n > r.size-r.offset {
		return nil, io.ErrUnexpectedEOF
	}
	if r.offset+n > maxGGUFMetadataSpan {
		return nil, ErrReadLimit
	}
	if r.offset < r.start || r.offset+n > r.start+int64(len(r.buf)) {
		length := max(n, 32<<10)
		length = min(length, r.size-r.offset)
		if r.readBytes+length > maxGGUFReadBytes {
			return nil, ErrReadLimit
		}
		b, err := r.files.ReadRange(r.ctx, r.name, r.offset, length)
		if err != nil {
			return nil, err
		}
		r.buf, r.start = b, r.offset
		r.readBytes += length
	}
	b := r.buf[r.offset-r.start : r.offset-r.start+n]
	r.offset += n
	return b, nil
}
func (r *ggufReader) u32() (uint32, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}
func (r *ggufReader) u64() (uint64, error) {
	b, err := r.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}
func (r *ggufReader) str() (string, error) {
	n, err := r.u64()
	if err != nil {
		return "", err
	}
	if n > maxDocumentBytes {
		return "", ErrReadLimit
	}
	b, err := r.take(int64(n))
	return string(b), err
}
func (r *ggufReader) skip(n uint64) error {
	if n > uint64(max(r.size-r.offset, 0)) {
		return io.ErrUnexpectedEOF
	}
	if n > maxGGUFMetadataSpan || r.offset+int64(n) > maxGGUFMetadataSpan {
		return ErrReadLimit
	}
	r.offset += int64(n)
	return nil
}

var ggufScalarSize = map[uint32]uint64{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}

func (r *ggufReader) skipValue(kind uint32) error {
	if size, ok := ggufScalarSize[kind]; ok {
		return r.skip(size)
	}
	if kind == 8 {
		n, err := r.u64()
		if err != nil {
			return err
		}
		return r.skip(n)
	}
	if kind != 9 {
		return errors.New("unknown GGUF metadata type")
	}
	element, err := r.u32()
	if err != nil {
		return err
	}
	count, err := r.u64()
	if err != nil {
		return err
	}
	if count > 1<<20 {
		return ErrReadLimit
	}
	if size, ok := ggufScalarSize[element]; ok {
		return r.skip(count * size)
	}
	if element != 8 {
		return errors.New("invalid GGUF array element type")
	}
	for i := uint64(0); i < count; i++ {
		if err := r.skipValue(8); err != nil {
			return err
		}
	}
	return nil
}

func readGGUF(ctx context.Context, files FileView, name string, size int64) (ggufMetadata, error) {
	r := ggufReader{ctx: ctx, files: files, name: name, size: size}
	var m ggufMetadata
	magic, err := r.take(4)
	if err != nil {
		return m, err
	}
	if string(magic) != "GGUF" {
		return m, errors.New("invalid GGUF magic")
	}
	version, err := r.u32()
	if err != nil {
		return m, err
	}
	if version != 2 && version != 3 {
		return m, errors.New("unsupported GGUF version")
	}
	m.tensors, err = r.u64()
	if err != nil {
		return m, err
	}
	if m.tensors == 0 {
		return m, errors.New("GGUF has no tensors")
	}
	count, err := r.u64()
	if err != nil {
		return m, err
	}
	if count > 4096 {
		return m, ErrReadLimit
	}
	for i := uint64(0); i < count; i++ {
		key, err := r.str()
		if err != nil {
			return m, err
		}
		kind, err := r.u32()
		if err != nil {
			return m, err
		}
		switch key {
		case "general.architecture", "general.type":
			if kind != 8 {
				return m, errors.New("GGUF identity must be a string")
			}
			value, err := r.str()
			if err != nil {
				return m, err
			}
			if key == "general.architecture" {
				m.architecture = value
			} else {
				m.kind = value
			}
		case "split.no", "split.count":
			size := ggufScalarSize[kind]
			if kind != 2 && kind != 4 && kind != 10 {
				return m, errors.New("GGUF split value must be unsigned")
			}
			b, err := r.take(int64(size))
			if err != nil {
				return m, err
			}
			var value uint64
			switch size {
			case 2:
				value = uint64(binary.LittleEndian.Uint16(b))
			case 4:
				value = uint64(binary.LittleEndian.Uint32(b))
			case 8:
				value = binary.LittleEndian.Uint64(b)
			}
			if key == "split.no" {
				m.splitNo = value
				m.hasSplit = true
			} else {
				m.splitCount = value
			}
		default:
			if err := r.skipValue(kind); err != nil {
				return m, err
			}
		}
	}
	return m, nil
}
