//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const addressActivationFileLimit = 1 << 20

// addressActivationDigest binds an offline copy to its complete filesystem
// contents, including directory modes and transport authorization lock files.
// The caller must keep the source offline for the duration of this operation.
func addressActivationDigest(ctx context.Context, dir string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !root.IsDir() || root.Mode().Perm()&0077 != 0 {
		return "", errors.New("activation source must be a private directory")
	}
	digest := sha256.New()
	field := func(data []byte) {
		_ = binary.Write(digest, binary.BigEndian, uint64(len(data)))
		_, _ = digest.Write(data)
	}
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("activation source contains a link or special file")
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		field([]byte(filepath.ToSlash(relative)))
		_ = binary.Write(digest, binary.BigEndian, uint32(info.Mode()))
		if info.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
			return errors.New("activation source changed while opening")
		}
		_ = binary.Write(digest, binary.BigEndian, uint64(opened.Size()))
		buffer := make([]byte, 32<<10)
		var total int64
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, readErr := file.Read(buffer)
			_, _ = digest.Write(buffer[:n])
			total += int64(n)
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		final, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !os.SameFile(info, final) || final.Mode() != info.Mode() || final.Size() != opened.Size() || total != opened.Size() || !final.ModTime().Equal(opened.ModTime()) {
			return errors.New("activation source changed while hashing")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func readAddressActivationBytes(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() <= 0 || before.Size() > addressActivationFileLimit {
		return nil, errors.New("activation record must be a bounded private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) || opened.Mode() != before.Mode() || opened.Size() != before.Size() {
		return nil, errors.New("activation record changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, addressActivationFileLimit+1))
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(opened, after) || after.Mode() != opened.Mode() || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || int64(len(data)) != opened.Size() {
		return nil, errors.New("activation record changed while reading")
	}
	return data, nil
}

func readAddressActivation(path string, value any) error {
	data, err := readAddressActivationBytes(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("activation record has trailing JSON")
	}
	return nil
}

// writeAddressActivation publishes a complete immutable record. Concurrent
// retries may confirm byte-identical publication but can never replace it.
func writeAddressActivation(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > addressActivationFileLimit {
		return errors.New("activation record exceeds size limit")
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return errors.New("activation record requires a parent directory without group or public write access")
	}
	confirm := func() error {
		existing, err := readAddressActivationBytes(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(data, existing) {
			return errors.New("activation record differs from existing publication")
		}
		return syncAddressActivationDirectory(parent)
	}
	if _, err := os.Lstat(path); err == nil {
		return confirm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(parent, ".activation-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Link(temporary, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return confirm()
		}
		return fmt.Errorf("publish activation record: %w", err)
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	return syncAddressActivationDirectory(parent)
}

func syncAddressActivationDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
