package cli

import (
	"errors"
	"os"
)

func clusterInstallPrivateFile(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	closeErr := f.Close()
	return errors.Join(writeErr, closeErr)
}
