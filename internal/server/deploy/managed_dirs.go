// managed_dirs.go owns the managed writable directories of a launch (ADR 0077):
// host directories the agent creates under its runtime mount root, mounts
// read-write, and the scheduler reserves on the disk they live on. There are
// two kinds, one per lifetime:
//
//   - a template's dirs (ManagedDir, MountPurposeManagedDir): state kept for
//     the deployment's lifetime, removed when the deployment is deleted, never
//     pruned;
//   - the KV disk tier (kv_disk.go, MountPurposeKVDisk): a cache kept while an
//     instance of the deployment runs on the node, pruned to its ceiling. It is
//     a deployment option, not a template declaration, because its size is
//     derived from the weights and the operator turns it on per deployment.
//
// Both reach the agent as a managed mount with no host source and the ceiling
// in SizeHint, and both are admitted on the same disk (ManagedDiskBytes).

package deploy

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	// ManagedDirRetainDeployment keeps a directory for as long as its
	// deployment exists: instances being removed, restarted or recreated (a
	// multi-node group re-formed, a failover) find what they left, and the
	// deployment's deletion removes it on every node.
	ManagedDirRetainDeployment = "deployment"
	// MaxManagedDirs bounds the directories one spec declares.
	MaxManagedDirs = 8
	// MaxManagedDirLimitBytes bounds one directory's declared size (64 TiB).
	MaxManagedDirLimitBytes = int64(1) << 46
)

// ManagedDir is one writable host directory a template declares. It is the one
// type of the declaration on every layer: the catalog YAML and JSON, the
// inference_specs.dirs column and the deployment's catalog selection snapshot.
type ManagedDir struct {
	// Name identifies the directory within the spec (proto.ValidManagedDirName).
	Name string `yaml:"name" json:"name"`
	// MountPath is where the container sees it: absolute, clean, not /.
	MountPath string `yaml:"mount_path" json:"mount_path"`
	// LimitBytes is the most the template writes there, in bytes like the
	// spec's other sizes (weight_bytes, shm_bytes). Admission
	// reserves it on every node of the deployment. Unlike the KV disk tier
	// the directory holds state, not a cache, so nothing prunes it: staying
	// within the size is the recipe's part.
	LimitBytes int64 `yaml:"limit_bytes" json:"limit_bytes"`
	// Retain is how long the directory lives; only
	// ManagedDirRetainDeployment, which import writes back when omitted.
	Retain string `yaml:"retain,omitempty" json:"retain,omitempty"`
}

// ManagedDirBytes is the space a launch's directories reserve on a node.
func ManagedDirBytes(dirs []ManagedDir) int64 {
	total := int64(0)
	for _, dir := range dirs {
		if dir.LimitBytes > 0 {
			total += dir.LimitBytes
		}
	}
	return total
}

// DecodeManagedDirs reads the inference_specs.dirs column. A blank column
// declares no directories and is not an error.
func DecodeManagedDirs(raw string) ([]ManagedDir, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var dirs []ManagedDir
	if err := json.Unmarshal([]byte(raw), &dirs); err != nil {
		return nil, fmt.Errorf("decode dirs: %w", err)
	}
	return dirs, nil
}

// EncodeManagedDirs renders the column; no directories is the blank column.
func EncodeManagedDirs(dirs []ManagedDir) (string, error) {
	if len(dirs) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(dirs)
	if err != nil {
		return "", fmt.Errorf("encode dirs: %w", err)
	}
	return string(raw), nil
}

// NormalizeManagedDirs trims and checks a declaration in place and writes the
// default retention back, so a stored declaration is always explicit. It
// checks the directories against each other; whether they collide with the
// spec's other container paths is the catalog's check.
func NormalizeManagedDirs(dirs []ManagedDir) error {
	if len(dirs) > MaxManagedDirs {
		return fmt.Errorf("dirs declares %d directories; at most %d are allowed", len(dirs), MaxManagedDirs)
	}
	names := map[string]struct{}{}
	for index := range dirs {
		dir := &dirs[index]
		dir.Name = strings.TrimSpace(dir.Name)
		dir.MountPath = strings.TrimSpace(dir.MountPath)
		dir.Retain = strings.ToLower(strings.TrimSpace(dir.Retain))
		if !proto.ValidManagedDirName(dir.Name) {
			return fmt.Errorf("dirs[%d] name %q must be 1-32 lower-case letters, digits or '-', starting and ending with a letter or digit", index, dir.Name)
		}
		if _, duplicate := names[dir.Name]; duplicate {
			return fmt.Errorf("dirs has duplicate name %q", dir.Name)
		}
		names[dir.Name] = struct{}{}
		if !strings.HasPrefix(dir.MountPath, "/") || path.Clean(dir.MountPath) != dir.MountPath || dir.MountPath == "/" {
			return fmt.Errorf("dir %s mount_path %q must be an absolute, clean container path other than /", dir.Name, dir.MountPath)
		}
		if dir.LimitBytes <= 0 || dir.LimitBytes > MaxManagedDirLimitBytes {
			return fmt.Errorf("dir %s limit_bytes must be between 1 and %d, got %d", dir.Name, MaxManagedDirLimitBytes, dir.LimitBytes)
		}
		if dir.Retain == "" {
			dir.Retain = ManagedDirRetainDeployment
		}
		if dir.Retain != ManagedDirRetainDeployment {
			return fmt.Errorf("dir %s retain %q is not supported (only %q)", dir.Name, dir.Retain, ManagedDirRetainDeployment)
		}
		for _, other := range dirs[:index] {
			if other.MountPath == dir.MountPath || mountPathContains(other.MountPath, dir.MountPath) || mountPathContains(dir.MountPath, other.MountPath) {
				return fmt.Errorf("dirs %s and %s have overlapping mount_paths %q and %q", other.Name, dir.Name, other.MountPath, dir.MountPath)
			}
		}
	}
	return nil
}

// managedDirMounts are the managed mounts a launch asks the agent for, one per
// declared directory. Source is empty: the host directory is the node's
// decision. group is the deployment's key (proto.ManagedDirGroupKey); a
// preview of a deployment not created yet has none and still shows the mounts.
func managedDirMounts(dirs []ManagedDir, group string) []proto.MountSpec {
	if len(dirs) == 0 {
		return nil
	}
	mounts := make([]proto.MountSpec, 0, len(dirs))
	for _, dir := range dirs {
		mounts = append(mounts, proto.MountSpec{
			Target: dir.MountPath, Purpose: proto.MountPurposeManagedDir,
			SizeHint:   strconv.FormatInt(dir.LimitBytes, 10),
			ManagedDir: &proto.ManagedDirRef{Group: group, Name: dir.Name},
		})
	}
	return mounts
}

// ManagedDiskBytes is what a launch reserves on the disk the node keeps
// managed directories on: its template's dirs (dirBytes, ManagedDirBytes) plus its KV disk tier's ceiling,
// which lives under the same runtime mount root (kvDiskLimitBytes; zero when
// the deployment names no tier directory or no ceiling can be derived).
func ManagedDiskBytes(dirBytes int64, q LaunchConfig, weightBytes int64) int64 {
	total := max(dirBytes, 0)
	if q.SharedKVDir != "" {
		total += kvDiskLimitBytes(q, weightBytes)
	}
	return total
}

// SnapshotDeclaresManagedDirs reports whether a stored catalog selection
// snapshot (deployments.template_snapshot) carries writable directories:
// only such a deployment's deletion has directories on nodes to release.
func SnapshotDeclaresManagedDirs(snapshot string) bool {
	selection, err := DecodeCatalogSelection([]byte(snapshot))
	return err == nil && len(selection.Dirs) > 0
}
