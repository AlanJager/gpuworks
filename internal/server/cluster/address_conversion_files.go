//go:build linux && dqlite

package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/canonical/go-dqlite/v3/client"
	"gopkg.in/yaml.v3"
)

type conversionFile struct {
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

func conversionPaths(source, backup, target string) (string, string, string, error) {
	paths := []string{source, backup, target}
	for index, path := range paths {
		if strings.TrimSpace(path) == "" {
			return "", "", "", errors.New("source, backup and candidate paths must be explicit")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", "", "", err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
		if err != nil {
			return "", "", "", fmt.Errorf("conversion directory parent must already exist: %w", err)
		}
		paths[index] = filepath.Join(parent, filepath.Base(absolute))
	}
	protected := []string{paths[0] + ".dqlite", paths[1], paths[2]}
	for a, left := range protected {
		for b, right := range protected {
			if a != b && (left == right || strings.HasPrefix(left, right+string(filepath.Separator))) {
				return "", "", "", errors.New("source, cold backup and candidate directories must not overlap")
			}
		}
	}
	for _, path := range []string{paths[0] + ".dqlite", paths[0] + ".runtime.lock"} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && strings.HasSuffix(path, ".runtime.lock") {
			continue
		}
		if err != nil {
			return "", "", "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", "", fmt.Errorf("conversion path must not be a symlink: %s", path)
		}
	}
	for _, path := range paths[1:] {
		if _, err := os.Lstat(path); err == nil {
			return "", "", "", fmt.Errorf("conversion output must not already exist: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", "", err
		}
	}
	return paths[0], paths[1], paths[2], nil
}

func conversionIdentity(dir string) (client.NodeInfo, error) {
	var info client.NodeInfo
	if _, err := os.Lstat(filepath.Join(dir, "join")); err == nil {
		return info, errors.New("source is still joining a cluster")
	} else if !errors.Is(err, os.ErrNotExist) {
		return info, err
	}
	read := func(name string, value any) error {
		path := filepath.Join(dir, name)
		stat, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() || stat.Size() == 0 || stat.Size() > 1<<20 {
			return fmt.Errorf("invalid conversion identity file: %s", name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return yaml.Unmarshal(data, value)
	}
	if err := read("info.yaml", &info); err != nil {
		return info, fmt.Errorf("read existing member identity: %w", err)
	}
	var cached []client.NodeInfo
	if err := read("cluster.yaml", &cached); err != nil {
		return info, err
	}
	if info.ID == 0 || len(cached) != 1 || cached[0].Address != info.Address || (cached[0].ID != 0 && cached[0].ID != info.ID) {
		return info, errors.New("source discovery cache is not an established single member")
	}
	return info, nil
}

// copyConversionDirectory copies only regular files/directories and verifies
// the complete read-back against the source. The source is never opened by
// dqlite or normalized; failed partial copies are kept for diagnosis.
func copyConversionDirectory(ctx context.Context, source, target string) (map[string]conversionFile, error) {
	if err := os.Mkdir(target, 0700); err != nil {
		return nil, err
	}
	expected := map[string]conversionFile{}
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, rel)
		meta := conversionFile{Mode: uint32(info.Mode()), Size: info.Size()}
		if info.IsDir() {
			meta.Size = 0
			if err := os.Mkdir(destination, info.Mode().Perm()); err != nil {
				return err
			}
		} else if info.Mode().IsRegular() {
			meta.SHA256, err = copyConversionFile(ctx, path, destination, info)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unsupported cold backup source type: %s", path)
		}
		expected[filepath.ToSlash(rel)] = meta
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, path := range []string{source, target} {
		actual, err := inspectConversionDirectory(ctx, path)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(expected, actual) {
			return nil, fmt.Errorf("cold backup source changed or read-back mismatch: %s", path)
		}
	}
	if err := syncConversionTree(ctx, target); err != nil {
		return nil, err
	}
	return expected, syncConversionDirectory(filepath.Dir(target))
}

func copyConversionFile(ctx context.Context, source, target string, before os.FileInfo) (_ string, resultErr error) {
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, before.Mode().Perm())
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, out.Close()) }()
	if err := out.Chmod(before.Mode().Perm()); err != nil {
		return "", err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(out, hash), conversionContextReader{ctx, in})
	if err != nil {
		return "", err
	}
	after, err := in.Stat()
	if err != nil {
		return "", err
	}
	if written != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", fmt.Errorf("cold source changed during backup: %s", source)
	}
	return hex.EncodeToString(hash.Sum(nil)), out.Sync()
}

func inspectConversionDirectory(ctx context.Context, dir string) (map[string]conversionFile, error) {
	files := map[string]conversionFile{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		meta := conversionFile{Mode: uint32(info.Mode()), Size: info.Size()}
		if info.IsDir() {
			meta.Size = 0
		} else if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			hash := sha256.New()
			n, err := io.Copy(hash, conversionContextReader{ctx, file})
			err = errors.Join(err, file.Close())
			if err != nil || n != info.Size() {
				return fmt.Errorf("read cold file %s: %w", path, errors.Join(err, io.ErrUnexpectedEOF))
			}
			meta.SHA256 = hex.EncodeToString(hash.Sum(nil))
		} else {
			return fmt.Errorf("unsupported cold backup source type: %s", path)
		}
		files[filepath.ToSlash(rel)] = meta
		return nil
	})
	return files, err
}

func writeConversionFile(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	return syncConversionDirectory(filepath.Dir(path))
}

func syncConversionDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func syncConversionTree(ctx context.Context, root string) error {
	var directories []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported converted file: %s", path)
		}
		return syncConversionDirectory(path)
	}); err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncConversionDirectory(directories[index]); err != nil {
			return err
		}
	}
	return nil
}

type conversionContextReader struct {
	ctx context.Context
	io.Reader
}

func (r conversionContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(buffer)
}
