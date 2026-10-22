package app

import (
	"context"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// imageArchiveLoad describes one runtime load of an image archive, whichever
// way the archive arrived. Load hands it to the runtime and reports the
// reference the archive actually carried, or "" when the runtime named none.
type imageArchiveLoad struct {
	// CanonicalRef is the catalog reference the image must answer to once the
	// load is done, whatever the publisher baked into the archive.
	CanonicalRef string
	// RuntimeName appears in the operator-facing message when the runtime
	// cannot do what the archive turned out to need.
	RuntimeName string
	// Loaded is the message recorded when the archive already carried the
	// canonical reference.
	Loaded string
	Load   func(context.Context) (string, error)
}

// loadImageArchive runs the one sequence every archive load shares: hand the
// archive to the runtime, tag the reference the archive actually carried when
// it differs from the canonical reference, confirm presence, and hydrate
// catalog metadata from the loaded image.
//
// A downloaded archive and one streamed through the management server differ
// only in how the bytes reach the runtime, so they share this: the tag
// fallback cannot end up implemented on one path and missing from the other.
// The returned status is terminal and successful; a failure is an error whose
// text is what the operator reads in the ledger.
func loadImageArchive(ctx context.Context, d runtime.Driver, load imageArchiveLoad) (proto.ImageNodeStatus, error) {
	if d == nil {
		return proto.ImageNodeStatus{}, errors.New("runtime driver is unavailable")
	}
	loadedRef, err := load.Load(ctx)
	if err != nil {
		return proto.ImageNodeStatus{}, fmt.Errorf("load image archive: %w", err)
	}
	present, err := d.ImageExists(ctx, load.CanonicalRef)
	if err != nil {
		return proto.ImageNodeStatus{}, fmt.Errorf("check loaded image: %w", err)
	}
	message := load.Loaded
	if (loadedRef != "" && loadedRef != load.CanonicalRef) || !present {
		// A pre-existing canonical tag can still name an older image. Always
		// replace it when the archive reports another reference; presence alone
		// cannot confirm that this load updated the catalog image.
		tagged, err := tagLoadedImage(ctx, d, load, loadedRef)
		if err != nil {
			return proto.ImageNodeStatus{}, err
		}
		message = tagged
	}
	status := proto.ImageNodeStatus{ImageRef: load.CanonicalRef, State: "present", Message: message}
	if inspector, ok := d.(runtime.ImageInspector); ok {
		if inspected, inspectErr := inspector.InspectImage(ctx, load.CanonicalRef); inspectErr == nil {
			status.ImageID = inspected.ID
		}
	}
	return status, nil
}

// tagLoadedImage maps an archive whose reference differs from the
// catalog one. It returns the message recorded on success, which names the
// reference the archive carried so the operator can tell a deliberately
// re-tagged image from a pasted address that turned out to hold something
// else entirely.
func tagLoadedImage(ctx context.Context, d runtime.Driver, load imageArchiveLoad, loadedRef string) (string, error) {
	if loadedRef == "" {
		return "", fmt.Errorf("image archive loaded but %s is not present, and the runtime did not report the reference the archive carried", load.CanonicalRef)
	}
	if loadedRef == load.CanonicalRef {
		// The archive carried exactly this reference and the runtime still does
		// not have it. Tagging a reference onto itself would only turn a clear
		// failure into a confusing one.
		return "", fmt.Errorf("image archive load completed but %s is not present", load.CanonicalRef)
	}
	tagger, ok := d.(runtime.ImageTagger)
	if !ok {
		return "", fmt.Errorf("image archive carried %s, not %s, and runtime %s cannot tag it", loadedRef, load.CanonicalRef, load.RuntimeName)
	}
	if err := tagger.TagImage(ctx, loadedRef, load.CanonicalRef); err != nil {
		return "", fmt.Errorf("tag %s as %s: %w", loadedRef, load.CanonicalRef, err)
	}
	present, err := d.ImageExists(ctx, load.CanonicalRef)
	if err != nil {
		return "", fmt.Errorf("check tagged image: %w", err)
	}
	if !present {
		return "", fmt.Errorf("image archive carried %s and tagging it as %s left the image absent", loadedRef, load.CanonicalRef)
	}
	return "image archive carried " + loadedRef + ", tagged as " + load.CanonicalRef, nil
}
