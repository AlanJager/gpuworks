package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// OpenSingleDatabase preserves the embedded node's private address on restart.
// It deliberately refuses a multi-member directory: cluster startup needs its
// own identity, transport and admission configuration, not a single-node
// fallback that silently bootstraps a replacement database.
func OpenSingleDatabase(ctx context.Context, dir, name string) (*Database, error) {
	type nodeInfo struct {
		ID      uint64 `yaml:"ID"`
		Address string `yaml:"Address"`
	}
	var info nodeInfo
	raw, err := os.ReadFile(filepath.Join(dir, "info.yaml"))
	switch {
	case err == nil:
		if err := yaml.Unmarshal(raw, &info); err != nil {
			return nil, fmt.Errorf("read database identity: %w", err)
		}
		membersRaw, err := os.ReadFile(filepath.Join(dir, "cluster.yaml"))
		if err != nil {
			return nil, fmt.Errorf("read database membership: %w", err)
		}
		var members []nodeInfo
		if err := yaml.Unmarshal(membersRaw, &members); err != nil {
			return nil, err
		}
		if info.ID == 0 || len(members) != 1 || (members[0].ID != 0 && members[0].ID != info.ID) || members[0].Address != info.Address {
			return nil, errors.New("database directory is not an established single-member cluster; use explicit cluster startup or recovery")
		}
	case errors.Is(err, os.ErrNotExist):
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if len(entries) != 0 {
			return nil, errors.New("database identity is missing from a nonempty directory; explicit recovery is required")
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		info.Address = listener.Addr().String()
		if err := listener.Close(); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	db, err := OpenDatabase(ctx, DatabaseConfig{Dir: dir, Address: info.Address, Name: name})
	if err != nil {
		return nil, err
	}
	// cluster.yaml is only a discovery cache, and can initially contain a
	// zero ID. Validate live membership before admitting any application work.
	members, err := db.Members(ctx)
	if err != nil || len(members) != 1 || members[0].ID != db.ID {
		_ = db.Close()
		if err != nil {
			return nil, fmt.Errorf("verify single-member database: %w", err)
		}
		return nil, errors.New("database has multiple members; use explicit cluster startup")
	}
	return db, nil
}
