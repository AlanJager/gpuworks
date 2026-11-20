//go:build linux && dqlite

package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
)

func copyReceivedMaterial(ctx context.Context, source, target *os.Root, name, digest string, size int64, centralStage bool) error {
	return copyReceivedMaterialWithLink(ctx, source, target, name, digest, size, centralStage, linkReceivedMaterial)
}

func copyReceivedMaterialWithLink(ctx context.Context, source, target *os.Root, name, digest string, size int64, centralStage bool, link func(context.Context, *os.Root, string, string) error) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name == "" || path.Clean(name) != name || path.IsAbs(name) || strings.HasPrefix(name, "../") || name == ".." || !validMaterialDigest(digest) || size <= 0 || size > maxMaterialReplicaBytes {
		return errors.New("invalid received material destination")
	}
	if err := ensureMaterialParent(target, name); err != nil {
		return err
	}
	if _, err := target.Lstat(name); err == nil {
		if err := verifyRootMaterial(ctx, target, name, digest, size); err != nil {
			return err
		}
		return syncRootDir(target, path.Dir(name))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sourceName := digest[:2] + "/" + digest
	before, err := source.Lstat(sourceName)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() != size {
		return errors.New("received source bytes are missing or differ")
	}
	input, err := source.Open(sourceName)
	if err != nil {
		return err
	}
	inputClosed := false
	closeInput := func() error {
		if inputClosed {
			return nil
		}
		inputClosed = true
		return input.Close()
	}
	defer func() { result = errors.Join(result, closeInput()) }()
	opened, err := input.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, opened) {
		return errors.New("received source changed before installation")
	}
	parent := path.Dir(name)
	stageParent := parent
	if centralStage {
		stageParent = materialStageDirectory
	}
	stageRoot, err := openPlainMaterialDirectory(target, stageParent)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, stageRoot.Close()) }()
	stageParentInfo, err := stageRoot.Stat(".")
	if err != nil {
		return err
	}
	finalRoot := stageRoot
	finalParentInfo := stageParentInfo
	if parent != stageParent {
		finalRoot, err = openPlainMaterialDirectory(target, parent)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, finalRoot.Close()) }()
		finalParentInfo, err = finalRoot.Stat(".")
		if err != nil {
			return err
		}
	}
	checkParents := func() error {
		return errors.Join(checkMaterialOwnedDirectory(target, stageParent, stageParentInfo), checkMaterialOwnedDirectory(target, parent, finalParentInfo))
	}
	if err := checkParents(); err != nil {
		return err
	}
	var stageBase string
	var output *os.File
	for attempt := 0; attempt < 3; attempt++ {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		stageBase = ".material-" + hex.EncodeToString(nonce[:])
		output, err = stageRoot.OpenFile(stageBase, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	outputClosed := false
	closeOutput := func() error {
		if outputClosed {
			return nil
		}
		outputClosed = true
		return output.Close()
	}
	defer func() { result = errors.Join(result, closeOutput()) }()
	original, err := output.Stat()
	if err != nil {
		return err
	}
	stageName := path.Join(stageParent, stageBase)
	stageRemoved := false
	cleanupStage := func() error {
		if stageRemoved {
			return nil
		}
		// Cleanup uses the held parent even when its external name changed.
		// It must never remove a new inode occupying the original stage name.
		if err := removeMaterialOwnedFile(stageRoot, stageBase, original); err != nil {
			return err
		}
		stageRemoved = true
		return syncRootDir(stageRoot, ".")
	}
	cleanupAttempted := false
	cleanup := func() error {
		if cleanupAttempted {
			return nil
		}
		cleanupAttempted = true
		return cleanupStage()
	}
	defer func() { result = errors.Join(result, cleanup()) }()
	hash := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(output, hash), materialContextReader{ctx, io.LimitReader(input, size+1)})
	stopped := ctx.Err()
	if copyErr != nil || count != size || hex.EncodeToString(hash.Sum(nil)) != digest || stopped != nil {
		var mismatch error
		if count != size || hex.EncodeToString(hash.Sum(nil)) != digest {
			mismatch = errors.New("received material changed during installation")
		}
		return errors.Join(copyErr, mismatch, stopped)
	}
	if err := errors.Join(output.Sync(), closeInput()); err != nil {
		return err
	}
	verified, err := output.Stat()
	if err != nil {
		return err
	}
	checkStage := func() error {
		return errors.Join(checkParents(), checkMaterialOwnedFile(stageRoot, stageBase, verified))
	}
	if err := checkStage(); err != nil {
		return err
	}
	linked := false
	linkErr := link(ctx, target, stageName, name)
	// Root.Link resolves two names. The checks on both sides detect replacement;
	// they are not an atomic rename/ownership primitive against arbitrary writers.
	afterLinkErr := checkStage()
	if linkErr != nil {
		if afterLinkErr != nil {
			return errors.Join(linkErr, afterLinkErr)
		}
		if centralStage && errors.Is(linkErr, syscall.EXDEV) {
			if err := errors.Join(cleanup(), closeOutput()); err != nil {
				return errors.Join(linkErr, err)
			}
			// A bind-mounted consumer can require a parent-local stage.
			return copyReceivedMaterialWithLink(ctx, source, target, name, digest, size, false, link)
		}
		if !errors.Is(linkErr, os.ErrExist) {
			return linkErr
		}
		if err := verifyRootMaterial(ctx, target, name, digest, size); err != nil {
			return errors.Join(linkErr, err)
		}
	} else {
		if afterLinkErr != nil {
			return afterLinkErr
		}
		if err := checkMaterialOwnedFile(finalRoot, path.Base(name), verified); err != nil {
			return err
		}
		// Identity alone cannot detect writes through another hard link.
		if err := verifyMaterialOwnedOutput(ctx, output, verified, digest, size); err != nil {
			return err
		}
		linked = true
	}
	var markerErr error
	if linked && receivedModelLogoOriginName(name, digest) {
		markerErr = recordReceivedMaterialOwnedLogoOrigin(target, stageRoot, finalRoot, stageParent, stageBase, name, stageParentInfo, finalParentInfo, verified)
	}
	// Once linked, finish durable cleanup despite request cancellation.
	stopped = ctx.Err()
	ownerErr := checkParents()
	if linked {
		ownerErr = errors.Join(ownerErr, checkMaterialOwnedFile(finalRoot, path.Base(name), verified))
	}
	return errors.Join(stopped, markerErr, ownerErr, cleanup(), syncRootDir(finalRoot, "."))
}

func linkReceivedMaterial(ctx context.Context, target *os.Root, stage, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return target.Link(stage, name)
}

func checkMaterialOwnedDirectory(root *os.Root, name string, original os.FileInfo) error {
	current, err := root.Lstat(name)
	if err == nil && (!current.IsDir() || !os.SameFile(original, current)) {
		err = errors.New("received material parent identity changed")
	}
	if err != nil {
		return &os.PathError{Op: "check", Path: name, Err: err}
	}
	return nil
}

func verifyMaterialOwnedOutput(ctx context.Context, file *os.File, original os.FileInfo, digest string, size int64) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	count, readErr := io.Copy(hash, materialContextReader{ctx, io.LimitReader(file, size+1)})
	current, statErr := file.Stat()
	var mismatch error
	if count != size || hex.EncodeToString(hash.Sum(nil)) != digest || (statErr == nil && (!os.SameFile(original, current) || current.Mode() != original.Mode() || current.Size() != original.Size() || !current.ModTime().Equal(original.ModTime()))) {
		mismatch = errors.New("published received material contents changed")
	}
	return errors.Join(readErr, statErr, mismatch, ctx.Err())
}

func checkMaterialOwnedFile(root *os.Root, name string, original os.FileInfo) error {
	current, err := root.Lstat(name)
	if err == nil && (!current.Mode().IsRegular() || !os.SameFile(original, current) || current.Size() != original.Size() || current.Mode() != original.Mode() || !current.ModTime().Equal(original.ModTime())) {
		err = errors.New("verified received material identity or contents changed")
	}
	if err != nil {
		return &os.PathError{Op: "check", Path: name, Err: err}
	}
	return nil
}

func removeMaterialOwnedFile(root *os.Root, name string, original os.FileInfo) error {
	current, err := root.Lstat(name)
	if err == nil && (!current.Mode().IsRegular() || !os.SameFile(original, current)) {
		err = errors.New("received material cleanup cannot confirm original file identity")
	}
	if err != nil {
		return &os.PathError{Op: "remove", Path: name, Err: err}
	}
	return root.Remove(name)
}

func recordReceivedMaterialOwnedLogoOrigin(target, stageRoot, finalRoot *os.Root, stageParent, stageBase, final string, stageParentInfo, finalParentInfo, verified os.FileInfo) (result error) {
	markerParent := path.Join(path.Dir(final), receivedModelLogoOriginDirectory)
	markerName := path.Base(final)
	if err := ensureMaterialParent(target, path.Join(markerParent, markerName)); err != nil {
		return err
	}
	markerRoot, err := openPlainMaterialDirectory(target, markerParent)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, markerRoot.Close()) }()
	markerParentInfo, err := markerRoot.Stat(".")
	if err != nil {
		return err
	}
	if markerParentInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("model logo origin directory is not private")
	}
	check := func() error {
		return errors.Join(
			checkMaterialOwnedDirectory(target, stageParent, stageParentInfo),
			checkMaterialOwnedDirectory(target, path.Dir(final), finalParentInfo),
			checkMaterialOwnedDirectory(target, markerParent, markerParentInfo),
			checkMaterialOwnedFile(stageRoot, stageBase, verified),
			checkMaterialOwnedFile(finalRoot, path.Base(final), verified),
		)
	}
	if err := check(); err != nil {
		return err
	}
	if err := target.Link(path.Join(stageParent, stageBase), path.Join(markerParent, markerName)); err != nil {
		return fmt.Errorf("record received model logo origin: %w", err)
	}
	if err := errors.Join(check(), checkMaterialOwnedFile(markerRoot, markerName, verified)); err != nil {
		return errors.Join(err, removeMaterialOwnedFile(markerRoot, markerName, verified), syncRootDir(markerRoot, "."))
	}
	if err := syncRootDir(markerRoot, "."); err != nil {
		return errors.Join(err, removeMaterialOwnedFile(markerRoot, markerName, verified), syncRootDir(markerRoot, "."))
	}
	return nil
}
