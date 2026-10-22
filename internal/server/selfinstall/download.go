package selfinstall

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxExtractedBinaryBytes bounds what an archive may write out. The real
// binary is on the order of 40 MiB; the limit only exists so a wrong or hostile
// artifact cannot fill the disk.
const maxExtractedBinaryBytes = 512 << 20

// downloadAsset fetches a release artifact into dir and verifies its sha256
// while streaming, so a corrupted transfer is rejected before anything is
// unpacked. It returns the path of the verified archive.
func (i *Installer) downloadAsset(ctx context.Context, endpoint string, want ReleaseFile, dir string) (string, error) {
	resp, err := i.get(ctx, endpoint)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := mkdirAll(dir, dirMode); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(want.Name)+".part*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("download %s: %w", endpoint, err)
	}
	if want.Size > 0 && written != want.Size {
		return "", fmt.Errorf("download %s: got %d bytes, the release manifest says %d", endpoint, written, want.Size)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); !strings.EqualFold(got, strings.TrimSpace(want.SHA256)) {
		return "", fmt.Errorf("download %s: sha256 %s does not match the release manifest (%s)", endpoint, got, want.SHA256)
	}
	final := filepath.Join(dir, filepath.Base(want.Name))
	if err := os.Rename(tmpName, final); err != nil {
		return "", err
	}
	return final, nil
}

// extractBinary pulls the server binary out of a release archive and writes it
// to dest with the executable bit set. Only the member named "gpuworks" is
// considered, and it is written by base name, so nothing in the archive can
// choose where it lands.
func extractBinary(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(archive), err)
	}
	defer gz.Close()

	reader := tar.NewReader(gz)
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(archive), err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != binaryName {
			continue
		}
		if found {
			return fmt.Errorf("%s contains more than one %s", filepath.Base(archive), binaryName)
		}
		found = true
		if err := writeExtracted(reader, dest, header.Size); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("%s does not contain a %s binary", filepath.Base(archive), binaryName)
	}
	return nil
}

func writeExtracted(reader io.Reader, dest string, size int64) error {
	if size > maxExtractedBinaryBytes {
		return fmt.Errorf("the %s binary in the archive is %d bytes, beyond the %d byte limit", binaryName, size, int64(maxExtractedBinaryBytes))
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".new*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	written, err := io.Copy(tmp, io.LimitReader(reader, maxExtractedBinaryBytes))
	if err != nil {
		tmp.Close()
		return err
	}
	if size > 0 && written != size {
		tmp.Close()
		return fmt.Errorf("the %s binary is truncated: %d of %d bytes", binaryName, written, size)
	}
	if err := tmp.Chmod(binaryMode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}
