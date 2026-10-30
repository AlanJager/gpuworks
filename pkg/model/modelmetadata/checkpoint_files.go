package modelmetadata

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func checkpoint(ctx context.Context, dir, format string, files FileView) (Entry, error) {
	// vLLM and SGLang's default loaders prefer safetensors, then bin, then pt.
	return checkpointWithExtensions(ctx, dir, format, files, []string{".safetensors", ".bin", ".pt"})
}

// An index owns its shard set. Loose files cannot repair a broken index, and
// alternative encodings must not be combined into one required set of weights.
func checkpointWithExtensions(ctx context.Context, dir, format string, files FileView, extensions []string) (Entry, error) {
	e := Entry{Path: dir, Format: format}
	items, err := files.ReadDir(ctx, dir)
	if err != nil {
		return e, err
	}
	for _, ext := range extensions {
		var indexes, weights []string
		for _, item := range items {
			name := path.Join(dir, item.Name)
			if strings.HasSuffix(item.Name, ext+".index.json") {
				indexes = append(indexes, name)
			} else if !item.IsDir && strings.HasSuffix(item.Name, ext) && !trainingCheckpointFile(item.Name) {
				weights = append(weights, name)
			}
		}
		if len(indexes) > 0 {
			e.Files, err = indexedCheckpointFiles(ctx, files, dir, indexes)
		} else if len(weights) > 0 {
			e.Files, err = unindexedCheckpointFiles(weights)
		} else {
			continue
		}
		if err != nil {
			return e, err
		}
		checkRequired(ctx, &e, files)
		return e, nil
	}
	return e, nil
}

func indexedCheckpointFiles(ctx context.Context, files FileView, dir string, indexes []string) ([]string, error) {
	slices.Sort(indexes)
	var selected []string
	for _, index := range indexes {
		shards, ok, err := readCheckpointIndex(ctx, files, index)
		if err == nil && !ok {
			err = fs.ErrNotExist
		}
		if err == nil && len(shards) == 0 {
			err = errors.New("empty weight_map")
		}
		if err != nil {
			return nil, &fs.PathError{Op: "read checkpoint index", Path: index, Err: err}
		}
		for i, target := range shards {
			shards[i], err = childPath(dir, target)
			if err != nil {
				return nil, &fs.PathError{Op: "resolve checkpoint shard", Path: index, Err: err}
			}
		}
		slices.Sort(shards)
		shards = slices.Compact(shards)
		if selected != nil && !slices.Equal(selected, shards) {
			return nil, fmt.Errorf("ambiguous checkpoint indexes %q and %q reference different shard sets", indexes[0], index)
		}
		selected = shards
	}
	return selected, nil
}

var checkpointShard = regexp.MustCompile(`^(.+)-(\d{5})-of-(\d{5})(\.(?:safetensors|bin|pt))$`)

const maxUnindexedCheckpointFiles = 10000

func unindexedCheckpointFiles(weights []string) ([]string, error) {
	required := make(map[string]bool, len(weights))
	groups := map[string]int{}
	for _, name := range weights {
		required[name] = true
		match := checkpointShard.FindStringSubmatch(path.Base(name))
		if match == nil {
			continue
		}
		part, _ := strconv.Atoi(match[2])
		count, _ := strconv.Atoi(match[3])
		if count > maxUnindexedCheckpointFiles {
			return nil, ErrReadLimit
		}
		if part < 1 || part > count {
			return nil, fmt.Errorf("invalid checkpoint shard number: %s", name)
		}
		group := path.Join(path.Dir(name), match[1]+match[4])
		if previous, ok := groups[group]; ok {
			if previous != count {
				return nil, fmt.Errorf("inconsistent checkpoint shard counts: %s", name)
			}
			continue
		}
		groups[group] = count
		for i := 1; i <= count; i++ {
			required[path.Join(path.Dir(name), fmt.Sprintf("%s-%05d-of-%05d%s", match[1], i, count, match[4]))] = true
			if len(required) > maxUnindexedCheckpointFiles {
				return nil, ErrReadLimit
			}
		}
	}
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// Training artifacts excluded by vLLM/SGLang's filter_files_not_needed_for_inference.
func trainingCheckpointFile(name string) bool {
	switch name {
	case "training_args.bin", "optimizer.bin", "optimizer.pt", "scheduler.pt", "scaler.pt":
		return true
	default:
		return false
	}
}
