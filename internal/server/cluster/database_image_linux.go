//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"errors"
	"net"

	"github.com/canonical/go-dqlite/v3/client"
)

// ReadDatabaseImage returns the named database as the member at address holds
// it: one SQLite file image that contains every write that member has applied
// and no part of an unfinished one. The member builds the image from a
// snapshot of its pages, so writers are not held while it is copied. The image
// says nothing about how far the member is behind its leader; a caller that
// needs the cluster's state reads it from the leader between two quorum proofs.
// The supplied dial must authenticate and pin the intended member transport.
func ReadDatabaseImage(ctx context.Context, address, name string, dial func(context.Context, string) (net.Conn, error)) (image []byte, result error) {
	if dial == nil {
		return nil, errors.New("a database image requires an authenticated dialer")
	}
	if address == "" || name == "" {
		return nil, errors.New("a database image requires a member address and a database name")
	}
	pinned := func(dialCtx context.Context, target string) (net.Conn, error) {
		if target != address {
			return nil, errors.New("a database image is read from the named member only")
		}
		return dial(dialCtx, target)
	}
	peer, err := client.New(ctx, address, client.WithDialFunc(pinned))
	if err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	defer func() {
		result = errors.Join(result, peer.Close())
		if result != nil {
			image = nil
		}
	}()
	files, err := peer.Dump(ctx, name)
	if err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	return databaseImage(name, files)
}

// databaseImage accepts exactly the main file with an empty write-ahead log:
// frames beside the image would be writes the image does not contain.
func databaseImage(name string, files []client.File) ([]byte, error) {
	if err := validateOfflineNativeFiles(name, files); err != nil {
		return nil, err
	}
	var image []byte
	for _, file := range files {
		if file.Name == name {
			image = file.Data
			continue
		}
		if len(file.Data) != 0 {
			return nil, errors.New("database image came with write-ahead log frames; it is not a complete image")
		}
	}
	if len(image) < 100 || !bytes.Equal(image[:16], []byte("SQLite format 3\x00")) {
		return nil, errors.New("database image is not a SQLite database")
	}
	return image, nil
}
