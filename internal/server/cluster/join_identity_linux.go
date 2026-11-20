//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/canonical/go-dqlite/v3/client"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// PrepareJoinIdentity publishes all discovery identity files before networking.
// The caller must hold the database runtime lease. Existing state is validated,
// never repaired or assigned a replacement ID. This grants no membership.
func PrepareJoinIdentity(ctx context.Context, dir, address string, seeds []string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	canonical, err := identity.NormalizeMemberAddress(address)
	if err != nil || canonical != address || len(seeds) == 0 || len(seeds) > 512 {
		return 0, errors.New("joining identity requires a canonical address and remote seeds")
	}
	nodes := make([]client.NodeInfo, 0, len(seeds))
	seen := make(map[string]bool, len(seeds))
	for _, seed := range seeds {
		canonical, err := identity.NormalizeMemberAddress(seed)
		if err != nil || canonical != seed || seed == address || seen[seed] {
			return 0, errors.New("joining identity requires unique canonical remote seeds")
		}
		seen[seed] = true
		nodes = append(nodes, client.NodeInfo{Address: seed})
	}
	parent := filepath.Dir(dir)
	if err := transportDirectory(parent); err != nil {
		return 0, err
	}
	if _, err := os.Lstat(dir); err == nil {
		id, err := readJoinIdentity(ctx, dir, address)
		if err != nil {
			return 0, err
		}
		// A prior rename may have succeeded while its directory sync failed.
		// An explicit retry confirms durability instead of merely seeing files.
		if err := errors.Join(transportSync(dir), transportSync(parent)); err != nil {
			return 0, err
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	id, err := generateMemberID(ctx, address)
	if err != nil {
		return 0, err
	}
	if id == 0 || id == 1 || id == bootstrapMemberID {
		return 0, errors.New("generated invalid joining member identity")
	}
	stage, err := os.MkdirTemp(parent, ".raft-identity-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(stage)
	info, err := yaml.Marshal(client.NodeInfo{ID: id, Address: address})
	if err != nil {
		return 0, err
	}
	cache, err := yaml.Marshal(nodes)
	if err != nil {
		return 0, err
	}
	if len(cache) > 128<<10 {
		return 0, errors.New("joining discovery cache exceeds file size limit")
	}
	for name, data := range map[string][]byte{"info.yaml": info, "cluster.yaml": cache, "join": {}} {
		if err := transportWrite(filepath.Join(stage, name), data); err != nil {
			return 0, err
		}
	}
	if err := transportSync(stage); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, dir, unix.RENAME_NOREPLACE); err != nil {
		return 0, err
	}
	if err := transportSync(parent); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return id, nil
}

func readJoinIdentity(ctx context.Context, dir, address string) (uint64, error) {
	if err := transportDirectory(dir); err != nil {
		return 0, err
	}
	decode := func(name string, out any) error {
		data, err := transportRead(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(out); err != nil {
			return err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return errors.New("joining identity has trailing YAML")
		}
		return nil
	}
	var info client.NodeInfo
	if err := decode("info.yaml", &info); err != nil {
		return 0, err
	}
	if info.ID == 0 || info.ID == 1 || info.ID == bootstrapMemberID || info.Address != address {
		return 0, errors.New("stored joining identity differs or is a bootstrap member")
	}
	var nodes []client.NodeInfo
	if err := decode("cluster.yaml", &nodes); err != nil {
		return 0, err
	}
	if len(nodes) == 0 || len(nodes) > 512 {
		return 0, errors.New("joining discovery cache is empty or exceeds capacity")
	}
	seen := make(map[string]bool, len(nodes))
	ids := make(map[uint64]bool, len(nodes))
	for _, node := range nodes {
		normalized, err := identity.NormalizeMemberAddress(node.Address)
		if err != nil || normalized != node.Address || seen[node.Address] || (node.ID != 0 && ids[node.ID]) || (node.Role != client.Voter && node.Role != client.StandBy && node.Role != client.Spare) {
			return 0, errors.New("invalid joining discovery cache")
		}
		if (node.ID == info.ID && node.Address != info.Address) || (node.Address == info.Address && node.ID != info.ID) {
			return 0, errors.New("joining discovery cache conflicts with local identity")
		}
		seen[node.Address], ids[node.ID] = true, true
	}
	if data, err := transportRead(filepath.Join(dir, "join")); err == nil {
		if len(data) != 0 {
			return 0, ErrInvalidJoinMarker
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	return info.ID, ctx.Err()
}

// ReconcileCommittedJoinMarker removes an empty upstream join marker only when
// the caller-supplied live membership already contains this directory's exact
// identity. It does not dial the cluster, change roles, or repair identity.
// The caller must hold the runtime lease and must not have this database open.
// A missing marker is already reconciled; every other mismatch is left untouched.
func ReconcileCommittedJoinMarker(ctx context.Context, dir string, members []Member) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := transportDirectory(dir); err != nil {
		return err
	}
	marker := filepath.Join(dir, "join")
	data, err := transportRead(marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) != 0 {
		return ErrInvalidJoinMarker
	}
	raw, err := transportRead(filepath.Join(dir, "info.yaml"))
	if err != nil {
		return err
	}
	var info client.NodeInfo
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&info); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("joining identity has trailing YAML")
	}
	id, err := readJoinIdentity(ctx, dir, info.Address)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.ID != id || member.Address != info.Address {
			continue
		}
		if err := os.Remove(marker); err != nil {
			return err
		}
		return errors.Join(transportSync(dir), ctx.Err())
	}
	return ErrJoinIdentityNotLive
}

// PublishedJoinSeeds reads the identity published before networking. It returns
// the stable member ID and the remote discovery addresses, and it does not
// create, repair, or delete files.
func PublishedJoinSeeds(ctx context.Context, dir string) (uint64, []string, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	raw, err := transportRead(filepath.Join(dir, "info.yaml"))
	if err != nil {
		return 0, nil, err
	}
	var info client.NodeInfo
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&info); err != nil {
		return 0, nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return 0, nil, errors.New("joining identity has trailing YAML")
	}
	id, err := readJoinIdentity(ctx, dir, info.Address)
	if err != nil {
		return 0, nil, err
	}
	raw, err = transportRead(filepath.Join(dir, "cluster.yaml"))
	if err != nil {
		return 0, nil, err
	}
	var nodes []client.NodeInfo
	decoder = yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&nodes); err != nil {
		return 0, nil, err
	}
	seeds := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.Address != info.Address {
			seeds = append(seeds, node.Address)
		}
	}
	if len(seeds) == 0 {
		return 0, nil, errors.New("joining identity requires a remote cluster member")
	}
	return id, seeds, nil
}
