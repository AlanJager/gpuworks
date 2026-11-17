package netinfo

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// roceGIDAddress repairs the IP association without rewriting the physical
// port's netdev or speed. Both the per-port GID and the current address on its
// named interface must agree; stale GIDs must not make a rail appear usable.
func roceGIDAddress(portDir string, ifaces []proto.NetInterface, physical string) *proto.RoCEAddress {
	entries, err := os.ReadDir(filepath.Join(portDir, "gid_attrs", "ndevs"))
	if err != nil {
		return nil
	}
	addresses := make(map[string][]string, len(ifaces))
	for _, iface := range ifaces {
		addresses[iface.Name] = iface.Addrs
	}
	type candidate struct {
		address  proto.RoCEAddress
		index    int
		physical bool
		v2       bool
	}
	var candidates []candidate
	for _, entry := range entries {
		index, err := strconv.Atoi(entry.Name())
		if err != nil || index < 0 {
			continue
		}
		gid := net.ParseIP(readString(filepath.Join(portDir, "gids", entry.Name())))
		if gid == nil || gid.IsUnspecified() || gid.To4() == nil {
			continue
		}
		name := readString(filepath.Join(portDir, "gid_attrs", "ndevs", entry.Name()))
		for _, addr := range addresses[name] {
			ip, _, err := net.ParseCIDR(addr)
			if err != nil || !ip.Equal(gid) {
				continue
			}
			kind := readString(filepath.Join(portDir, "gid_attrs", "types", entry.Name()))
			candidates = append(candidates, candidate{
				address: proto.RoCEAddress{NetDev: name, CIDR: addr}, index: index,
				physical: name == physical, v2: strings.EqualFold(kind, "RoCE v2"),
			})
			break
		}
	}
	// Planning needs IPv4; a physical link-local IPv6 GID must not hide
	// a usable bridge address. Keep a verified physical IPv4 association.
	// Among otherwise equivalent
	// choices prefer RoCE v2, then the numeric GID index, not filename order.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].physical != candidates[j].physical {
			return candidates[i].physical
		}
		if candidates[i].v2 != candidates[j].v2 {
			return candidates[i].v2
		}
		return candidates[i].index < candidates[j].index
	})
	if len(candidates) == 0 {
		return nil
	}
	return &candidates[0].address
}
