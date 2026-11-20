package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ErrBackupSourceChanged means the live files no longer match the backup.
// Preview must not rewrite them to hide that.
var ErrBackupSourceChanged = errors.New("backup source changed and must be confirmed again")

type backupManifest struct {
	Files []backupFile `json:"files"`
}

type backupFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// CreateMaterialBackup copies the named source files into dest. The backup
// holds the bytes, and the manifest only identifies them.
func CreateMaterialBackup(dest string, sources map[string]string) error {
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	manifest := backupManifest{Files: make([]backupFile, 0, len(sources))}
	for name, source := range sources {
		if err := copyBackupFile(dest, name, source); err != nil {
			return err
		}
		sum, err := hashFile(filepath.Join(dest, name))
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, backupFile{Name: name, SHA256: sum})
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "manifest.json"), append(raw, '\n'), 0600)
}

// PreviewMaterialBackup reads a backup without writing the source and without
// starting business. Fewer than two voters, or a source that no longer matches,
// leaves business closed.
func PreviewMaterialBackup(voters int, sourceDir, backupDir string) (bool, error) {
	if voters < 2 {
		return false, nil
	}
	manifestPath := filepath.Join(backupDir, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return false, err
	}
	var manifest backupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false, err
	}
	for _, file := range manifest.Files {
		sum, err := hashFile(filepath.Join(sourceDir, file.Name))
		if err != nil {
			return false, err
		}
		if sum != file.SHA256 {
			return false, ErrBackupSourceChanged
		}
	}
	return false, nil
}

func copyBackupFile(dest, name, source string) error {
	if name != filepath.Base(name) || name == "" || name == "." || name == ".." || name == "manifest.json" {
		return errors.New("backup file name must be a single path element")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(filepath.Join(dest, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
