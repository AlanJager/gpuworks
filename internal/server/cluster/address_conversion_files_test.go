//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type conversionTestReader func([]byte) (int, error)

func (r conversionTestReader) Read(buffer []byte) (int, error) { return r(buffer) }

func TestSingleAddressConversionCopyCancellation(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input := strings.NewReader(strings.Repeat("cold data", 100000))
	reader := conversionContextReader{ctx, conversionTestReader(func(buffer []byte) (int, error) {
		count, err := input.Read(buffer)
		cancel()
		return count, err
	})}
	var copied bytes.Buffer
	count, err := io.Copy(&copied, reader)
	if !errors.Is(err, context.Canceled) || count == 0 || input.Len() == 0 || int64(copied.Len()) != count {
		t.Fatalf("copy did not stop on cancellation between chunks: copied=%d remaining=%d err=%v", count, input.Len(), err)
	}
	if err := syncConversionTree(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled directory sync = %v", err)
	}
}

func TestSingleAddressConversionRefusesLinkedState(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	source, _ := conversionSource(t, ctx, root)
	outside := filepath.Join(root, "external-state")
	if err := os.WriteFile(outside, []byte("outside must remain untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(source+".dqlite", "unexpected-link")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	config := newConversionTestCA(t).config(t, filepath.Join(root, "candidate"), "127.0.0.1", 2)
	backup := filepath.Join(root, "backup")
	if _, err := PrepareSingleMemberAddress(ctx, source, backup, config); err == nil {
		t.Fatal("conversion followed a state symlink")
	}
	if _, err := os.Stat(config.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe state produced a candidate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backup, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial backup was declared complete: %v", err)
	}
	if value, err := os.Readlink(linked); err != nil || value != outside {
		t.Fatalf("source symlink changed: %s, %v", value, err)
	}
	if value, err := os.ReadFile(outside); err != nil || string(value) != "outside must remain untouched" {
		t.Fatalf("external state changed: %s, %v", value, err)
	}
	alias := filepath.Join(root, "aliased.db")
	if err := os.Symlink(source+".dqlite", alias+".dqlite"); err != nil {
		t.Fatal(err)
	}
	config.Name = filepath.Base(alias)
	if _, err := PrepareSingleMemberAddress(ctx, alias, filepath.Join(root, "other-backup"), config); err == nil {
		t.Fatal("conversion followed a linked source root")
	}
}
