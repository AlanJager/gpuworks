package serverconfig

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var errOutboundSnapshotChanged = errors.New("outbound configuration changed while being read")

// readOutboundSnapshot binds bounded content to its descriptor and pathname.
// The watcher permits existing symlinks; the settings writer checks the path
// separately before replacing a regular configuration file.
func readOutboundSnapshot(path string) (raw []byte, info os.FileInfo, err error) {
	f, err := openOutboundConfiguration(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		err = errors.Join(err, f.Close())
		if err != nil {
			raw, info = nil, nil
		}
	}()
	before, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: configuration must be a regular file", ErrOutboundPolicyInvalid)
	}
	if before.Size() > maxOutboundConfigBytes {
		return nil, nil, fmt.Errorf("%w: configuration exceeds the runtime size limit", ErrOutboundPolicyInvalid)
	}
	raw, err = io.ReadAll(io.LimitReader(f, maxOutboundConfigBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > maxOutboundConfigBytes {
		return nil, nil, fmt.Errorf("%w: configuration exceeds the runtime size limit", ErrOutboundPolicyInvalid)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !sameOutboundConfigSnapshot(before, after) {
		return nil, nil, errOutboundSnapshotChanged
	}
	current, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !sameOutboundConfigSnapshot(before, current) {
		return nil, nil, errOutboundSnapshotChanged
	}
	return raw, current, nil
}
