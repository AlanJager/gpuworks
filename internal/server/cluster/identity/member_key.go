package identity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CreateMemberKey writes a new identity exactly once in an existing private
// directory. The join coordinator must do this before its first exchange and
// use LoadMemberKey on restart. This function never replaces an existing key,
// including a damaged one, and never repairs missing state automatically.
func CreateMemberKey(path string) (_ []byte, resultErr error) {
	if err := privateKeyDirectory(path); err != nil {
		return nil, err
	}
	key, err := NewMemberKey()
	if err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".member-key-")
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, os.Remove(temporary.Name()))
	}()
	_, writeErr := temporary.Write(key)
	if err := errors.Join(writeErr, temporary.Sync(), temporary.Close()); err != nil {
		return nil, err
	}
	// A hard-link publication is atomic and cannot overwrite another creator.
	// A crash before this point exposes no partially written identity at path.
	if err := os.Link(temporary.Name(), path); err != nil {
		return nil, err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("member key published but directory durability unknown: %w", err)
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		return nil, fmt.Errorf("member key published but directory durability unknown: %w", err)
	}
	return key, nil
}

// LoadMemberKey fails closed on missing, partial, linked or exposed material.
// Callers must not generate a replacement after an arbitrary loading failure.
func LoadMemberKey(path string) ([]byte, error) {
	if err := privateKeyDirectory(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 || info.Size() > 64<<10 {
		return nil, errors.New("member key must be a private regular file")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if _, err := parseKey(key); err != nil {
		return nil, err
	}
	return key, nil
}

func privateKeyDirectory(path string) error {
	if filepath.Base(path) == "." || filepath.Base(path) == string(filepath.Separator) {
		return errors.New("member key file path is required")
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("member key parent must be an existing private directory")
	}
	return nil
}
