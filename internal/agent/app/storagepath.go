package app

import (
	"fmt"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/proto"
)

// storageTarget is where a model lives, in the explicit two-field form the
// server sends: exactly one of Abs and Rel is set.
type storageTarget struct {
	// Abs is an absolute directory on this host (a node's own row).
	Abs string
	// Rel is relative to the storage root (a shared storage's row, or one
	// not yet written).
	Rel string
}

// taskTarget reads where a model file task's model lives: the explicit Path
// and RelPath, or, from a server that predates them, the old TargetPath.
func taskTarget(task proto.ModelFileTask) storageTarget {
	if task.Path != "" || task.RelPath != "" {
		return storageTarget{Abs: task.Path, Rel: task.RelPath}
	}
	return legacyTaskTarget(task.TargetPath)
}

// legacyTaskTarget is the compatibility reading of a path in the form that
// predates the Path/RelPath split, where one field held either kind and the
// agent told them apart by filepath.IsAbs. It serves a task from an older
// server (ModelFileTask.TargetPath) and the model.write stream header, which
// still carries one path. Remove it, with ModelFileTask.TargetPath, once no
// server older than the release that introduced Path/RelPath is supported;
// the stream header should then name its kind the same way.
func legacyTaskTarget(target string) storageTarget {
	if filepath.IsAbs(target) {
		return storageTarget{Abs: target}
	}
	return storageTarget{Rel: target}
}

// resolveStorageTarget is the one place a model's target becomes a directory
// on this node. An absolute one (a local storage's row holds the node's own
// directory, and docker bind mounts need absolute paths) is used as is, a
// relative one (a shared storage's row, or a row not yet written) must name
// something below root, so ".", "/" or "../x" can never point at the storage
// itself or leave it.
func resolveStorageTarget(root string, target storageTarget) (string, error) {
	switch {
	case target.Abs != "" && target.Rel != "":
		return "", fmt.Errorf("target path: both an absolute path %q and a relative path %q are set", target.Abs, target.Rel)
	case target.Abs != "":
		if !filepath.IsAbs(target.Abs) {
			return "", fmt.Errorf("target path: %q is not absolute", target.Abs)
		}
		return filepath.Clean(target.Abs), nil
	}
	dir, err := proto.JoinWithin(root, target.Rel)
	if err != nil {
		return "", fmt.Errorf("target path: %w", err)
	}
	return dir, nil
}

// stagingWorkDir resolves the staging directory a task asks to assemble a
// model in. It is deleted when the install finds the model already in place
// (and cleaned up around the install), so it must be a directory this
// control plane owns: below <root>/.gpuworks/incoming/<control plane>/, never
// the incoming directory itself or anything outside it.
func stagingWorkDir(root, staging string) (string, error) {
	dir, err := proto.JoinWithin(root, staging)
	if err != nil {
		return "", fmt.Errorf("staging path: %w", err)
	}
	incoming := filepath.Join(root, proto.StorageMetaDir, proto.StorageIncomingDir)
	rel, err := filepath.Rel(incoming, dir)
	if err != nil || !pathWithin(incoming, dir) || rel == "." ||
		// <control plane id>/<model path>: the first element is the owner's own directory.
		filepath.Dir(rel) == "." {
		return "", fmt.Errorf("staging path %q is not a directory below %s", staging, incoming)
	}
	return dir, nil
}
