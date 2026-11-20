//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"

	"github.com/canonical/go-dqlite/v3/client"
	"github.com/canonical/go-dqlite/v3/driver"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// OpenRemoteExistingReadSQL creates a private, lazy SQL pool for one voter.
// dial must authenticate the expected member and pin its public-key identity;
// restricting addresses here does not replace that authentication. ctx owns
// the entire pool lifetime. The caller must join cleanup before releasing any
// resources borrowed by dial. No local native node or database is started.
func OpenRemoteExistingReadSQL(ctx context.Context, member Member, name string, dial func(context.Context, string) (net.Conn, error)) (*sql.DB, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	address, err := identity.NormalizeMemberAddress(member.Address)
	if err != nil || address != member.Address || member.ID == 0 || member.Role != "voter" {
		return nil, nil, errors.New("remote existing SQL requires an exact canonical voter identity")
	}
	if err := validateExistingReadName(name); err != nil {
		return nil, nil, err
	}
	if dial == nil {
		return nil, nil, errors.New("remote existing SQL requires an authenticated pinned dialer")
	}
	seeds := client.NewInmemNodeStore()
	if err := seeds.Set(ctx, []client.NodeInfo{{ID: member.ID, Address: address, Role: client.Voter}}); err != nil {
		return nil, nil, err
	}
	privateDriver, err := driver.New(seeds, driver.WithDialFunc(func(ctx context.Context, requested string) (net.Conn, error) {
		// The SDK can learn a different endpoint during leader discovery. Every
		// actual attempt must retain the tuple the caller authorized.
		if requested != address {
			return nil, fmt.Errorf("remote existing SQL rejects changed endpoint %q", requested)
		}
		return dialQuorumSQLSocket(ctx, requested, func(ctx context.Context, target string) (net.Conn, error) {
			conn, err := dial(ctx, target)
			if conn != nil {
				conn = wrapExistingReadSQLConnection(conn)
			}
			// Even a socket returned with an error belongs to the original
			// owner, which closes it and joins its actual acknowledgement.
			return conn, err
		})
	}))
	if err != nil {
		return nil, nil, err
	}
	connector, err := privateDriver.OpenConnector(name)
	if err != nil {
		return nil, nil, err
	}
	pool, cleanup, err := openQuorumSQL(ctx, connector)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	return pool, cleanup, nil
}
