package tokenizer

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func tokenizerCachePath(rawURL string) string {
	root, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	// Hash the full source URL so revisions, mirrors and credentials cannot
	// collide or expose secrets in cache filenames.
	return filepath.Join(root, "gpuworks", "tokenizers", fmt.Sprintf("%x.json", sha256.Sum256([]byte(rawURL))))
}

func cachedTokenizer(path string) *Tokenizer {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	// References can name mutable main branches. Bound reuse to a day rather
	// than treating a successful fetch as an immutable tokenizer identity.
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxTokenizerBytes || time.Since(info.ModTime()) < 0 || time.Since(info.ModTime()) >= 24*time.Hour {
		return nil
	}
	tok, err := LoadFile(io.LimitReader(f, maxTokenizerBytes))
	if err != nil {
		return nil
	}
	return tok
}

func cacheTokenizer(path string, data []byte) {
	// A read-only or full cache must not turn a valid download into a failure.
	if path == "" || os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tokenizer-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	closeErr := f.Close()
	if err == nil && closeErr == nil {
		// Readers and concurrent writers see a complete, validated file.
		_ = os.Rename(f.Name(), path)
	}
}
