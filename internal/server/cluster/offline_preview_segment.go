//go:build linux && dqlite

package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// These transient facts retain bytes that the native open-segment normalizer
// can truncate. They cannot be reconstructed after that normalization.
type offlinePreviewSegmentBytes struct {
	info   os.FileInfo
	length int64
	digest [sha256.Size]byte
	record bool
}

// offlinePreviewPreserveSegments runs only on the exclusively owned scratch.
// The native parser decides format/CRC validity, but can silently drop a bad
// nonzero open suffix even with auto-recovery disabled. Compare its physical
// normalization before any election/replay can create or collect log files.
func offlinePreviewPreserveSegments(ctx context.Context, dir string) error {
	opens, protected, err := offlinePreviewSegmentInputs(ctx, dir)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := describeLastRaftEntry(ctx, dir); err != nil {
		return fmt.Errorf("offline preview native segment validation: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return offlinePreviewVerifySegments(ctx, dir, opens, protected)
}

func offlinePreviewSegmentInputs(ctx context.Context, dir string) (map[string]offlinePreviewSegmentBytes, map[string]offlinePreviewSegmentBytes, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	opens, protected := map[string]offlinePreviewSegmentBytes{}, map[string]offlinePreviewSegmentBytes{}
	snapshots := map[string]bool{}
	for _, entry := range entries {
		kind, err := offlinePreviewSegmentKind(entry.Name())
		if err != nil {
			return nil, nil, err
		}
		if kind == "" {
			continue
		}
		if kind == "snapshot" {
			snapshots[entry.Name()] = true
		}
		fact, err := offlinePreviewHashSegment(ctx, filepath.Join(dir, entry.Name()), kind == "open")
		if err != nil {
			return nil, nil, err
		}
		if kind == "open" {
			opens[entry.Name()] = fact
		} else {
			protected[entry.Name()] = fact
		}
	}
	for name := range snapshots {
		other := strings.TrimSuffix(name, ".meta")
		if other == name {
			other += ".meta"
		}
		if !snapshots[other] {
			return nil, nil, fmt.Errorf("offline preview incomplete native snapshot pair: %s", name)
		}
	}
	return opens, protected, nil
}

func offlinePreviewVerifySegments(ctx context.Context, dir string, opens, protected map[string]offlinePreviewSegmentBytes) error {
	for name, before := range protected {
		after, err := offlinePreviewHashSegment(ctx, filepath.Join(dir, name), false)
		if err != nil {
			return err
		}
		if !os.SameFile(before.info, after.info) || before.length != after.length || before.digest != after.digest {
			return fmt.Errorf("offline preview native normalization changed original file: %s", name)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	candidates := map[string]offlinePreviewSegmentBytes{}
	for _, entry := range entries {
		name := entry.Name()
		kind, err := offlinePreviewSegmentKind(name)
		if err != nil {
			return err
		}
		switch kind {
		case "closed":
			if _, existed := protected[name]; existed {
				continue
			}
			fact, err := offlinePreviewHashSegment(ctx, filepath.Join(dir, name), true)
			if err != nil {
				return err
			}
			candidates[name] = fact
		case "open":
			// The canonical parser must account for every original open, even if
			// snapshot filtering would otherwise ignore it.
			return fmt.Errorf("offline preview native normalization left an open segment: %s", name)
		case "snapshot":
			if _, existed := protected[name]; !existed {
				return fmt.Errorf("offline preview native normalization created a snapshot: %s", name)
			}
		}
	}
	for name, before := range opens {
		if !before.record {
			continue
		} // Only zero allocation/legal header-only bytes.
		matched := ""
		for candidate, after := range candidates {
			if before.length == after.length && before.digest == after.digest {
				matched = candidate
				break
			}
		}
		if matched == "" {
			return fmt.Errorf("offline preview native normalization lost nonzero segment bytes: %s", name)
		}
		delete(candidates, matched) // A closed file cannot stand in for two originals.
	}
	if len(candidates) != 0 {
		return errors.New("offline preview native normalization produced unmatched segment bytes")
	}
	return ctx.Err()
}

// Hash complete protected files, or an open/new closed file's meaningful
// prefix. The first pass locates the last nonzero byte; the second hashes that
// exact prefix. No record decoder/checksum policy is duplicated here.
func offlinePreviewHashSegment(ctx context.Context, path string, prefix bool) (fact offlinePreviewSegmentBytes, resultErr error) {
	f, err := os.Open(path)
	if err != nil {
		return fact, err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	fact.info, err = f.Stat()
	if err != nil {
		return fact, err
	}
	if !fact.info.Mode().IsRegular() {
		return fact, errors.New("offline preview native persistence is not a regular file")
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var offset, last int64
	for {
		if err := ctx.Err(); err != nil {
			return fact, err
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			if prefix {
				for i := n - 1; i >= 0; i-- {
					if buffer[i] != 0 {
						last = offset + int64(i) + 1
						break
					}
				}
			} else {
				_, _ = hash.Write(buffer[:n])
			}
			offset += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fact, readErr
		}
	}
	if offset != fact.info.Size() {
		return fact, errors.New("offline preview native file changed while hashing")
	}
	fact.length = offset
	if prefix {
		// Preserve the complete version word, even if its last bytes are zero.
		// Empty/allzero/known legal header-only opens have no log record. The
		// actual native parser still has to accept them before this is used.
		fact.length = last
		if offset >= 8 && fact.length < 8 {
			fact.length = 8
		}
		fact.record = last > 8
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fact, err
		}
		if !fact.record && offset >= 8 {
			header := make([]byte, 8)
			if _, err := io.ReadFull(f, header); err != nil {
				return fact, err
			}
			format := binary.LittleEndian.Uint64(header)
			if format != 0 && format != 1 {
				return fact, errors.New("offline preview unsupported empty native segment format")
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return fact, err
			}
		}
		remaining := fact.length
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				return fact, err
			}
			size := int64(len(buffer))
			if remaining < size {
				size = remaining
			}
			n, err := io.ReadFull(f, buffer[:int(size)])
			if err != nil {
				return fact, err
			}
			_, _ = hash.Write(buffer[:n])
			remaining -= int64(n)
		}
	}
	copy(fact.digest[:], hash.Sum(nil))
	after, err := f.Stat()
	if err != nil {
		return fact, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return fact, err
	}
	if !os.SameFile(fact.info, after) || !os.SameFile(after, pathInfo) || after.Size() != fact.info.Size() || !after.ModTime().Equal(fact.info.ModTime()) {
		return fact, errors.New("offline preview native file identity changed while hashing")
	}
	return fact, nil
}

func offlinePreviewSegmentKind(name string) (string, error) {
	parse := func(raw string) (uint64, bool) {
		value, err := strconv.ParseUint(raw, 10, 64)
		return value, err == nil && strconv.FormatUint(value, 10) == raw
	}
	if strings.HasPrefix(name, "open-") {
		if _, ok := parse(strings.TrimPrefix(name, "open-")); !ok {
			return "", fmt.Errorf("offline preview noncanonical native open filename: %s", name)
		}
		return "open", nil
	}
	if strings.HasPrefix(name, "snapshot-") {
		base := strings.TrimSuffix(strings.TrimPrefix(name, "snapshot-"), ".meta")
		parts := strings.Split(base, "-")
		if len(parts) != 3 {
			return "", fmt.Errorf("offline preview noncanonical snapshot filename: %s", name)
		}
		for _, part := range parts {
			if _, ok := parse(part); !ok {
				return "", fmt.Errorf("offline preview noncanonical snapshot filename: %s", name)
			}
		}
		return "snapshot", nil
	}
	// Match only names the pinned C unsigned scanf consumes completely. Field
	// width includes a sign, while leading ASCII whitespace does not count.
	if !offlinePreviewClosedSegmentName.MatchString(name) {
		return "", nil
	}
	var first, last uint64
	if n, _ := fmt.Sscanf(name, "%d-%d", &first, &last); n != 2 || len(name) != 33 || first == 0 || last < first || fmt.Sprintf("%016d-%016d", first, last) != name {
		return "", fmt.Errorf("offline preview noncanonical closed filename: %s", name)
	}
	return "closed", nil
}

// UV__CLOSED_TEMPLATE is %016llu-%016llu followed by %n. A signed value
// receives at most 15 digits; scanf's unsigned conversion also accepts minus.
var offlinePreviewClosedSegmentName = regexp.MustCompile(`^[ \t\r\n\v\f]*(?:[+-]?[0-9]{1,15}|[0-9]{16})-[ \t\r\n\v\f]*(?:[+-]?[0-9]{1,15}|[0-9]{16})$`)
