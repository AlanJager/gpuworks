package extpkg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/server/extpkg/archive"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// decideFunc accepts or refuses a verified package with approval hash
// hash, returning the control file to write when it is made current.
type decideFunc func(v *Verified, hash extapi.Digest) (layout.Control, error)

// placement is a package that was verified, accepted and unpacked to its
// version directory, and is not yet current.
type placement struct {
	v        *Verified
	hash     extapi.Digest
	ctl      layout.Control
	features FeatureOwners
	// unpacked reports that this placement created the version directory;
	// one that is abandoned removes it again.
	unpacked  bool
	unchanged bool
}

// unpack reads, verifies and accepts (decide) a package under the data
// directory's lock and unpacks it to its version directory without making
// it current. A version already unpacked with the same signed manifest is
// used as it is; one with a different manifest is refused
// (ErrorPluginVersionConflict). Nothing a running version uses changes.
func (h *Host) unpack(r io.Reader, features FeatureOwners, decide decideFunc) (*placement, error) {
	unlock, err := h.Dir.Lock()
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock() }()
	return h.unpackLocked(r, features, decide)
}

// unpackLocked is unpack with the data directory's lock held.
func (h *Host) unpackLocked(r io.Reader, features FeatureOwners, decide decideFunc) (*placement, error) {
	// Under the lock no other install is running: what an interrupted one
	// left behind goes first.
	if err := h.Dir.Cleanup(); err != nil {
		return nil, err
	}
	stage, err := h.Dir.Stage()
	if err != nil {
		return nil, err
	}
	defer func() { _ = stage.Abort() }()
	c, err := archive.Read(r, stage, h.limits())
	if err != nil {
		return nil, err
	}
	v, err := h.Verifier.Verify(c, Install)
	if err != nil {
		return nil, err
	}
	m := v.Manifest
	if err := h.checkPlaceable(m, features); err != nil {
		return nil, err
	}
	hash, err := ApprovalHash(v)
	if err != nil {
		return nil, err
	}
	ctl, err := decide(v, hash)
	if err != nil {
		return nil, err
	}
	p := &placement{v: v, hash: hash, ctl: ctl, features: features}
	switch existing, err := os.ReadFile(filepath.Join(h.Dir.Version(m.ID, m.Version), archive.ManifestMember)); {
	case err == nil:
		if !bytes.Equal(existing, c.Manifest) {
			return nil, messages.NewProductError(messages.ErrorPluginVersionConflict,
				fmt.Sprintf("%s %s is installed with other contents", m.ID, m.Version),
				map[string]any{"id": m.ID, "version": m.Version})
		}
		p.unchanged = true
	case errors.Is(err, fs.ErrNotExist):
		if err := stage.WriteSignature(c); err != nil {
			return nil, err
		}
		exec := map[string]bool{}
		for _, f := range m.Files {
			exec[f.Path] = f.Exec
		}
		if err := stage.Seal(exec); err != nil {
			return nil, err
		}
		if err := stage.Commit(h.Dir, m.ID, m.Version); err != nil {
			return nil, err
		}
		p.unpacked = true
	default:
		return nil, err
	}
	return p, nil
}

// checkPlaceable refuses a package whose data a newer build wrote or that
// declares a feature key someone else provides.
func (h *Host) checkPlaceable(m *extapi.Manifest, features FeatureOwners) error {
	if err := h.CheckDataMarker(m); err != nil {
		return err
	}
	return checkFeatureOwners(m, features)
}

// activate makes p's version current and writes its control file under
// the data directory's lock. confirm runs first, under the lock, since
// anything may have changed since unpack: it refuses the package or
// returns the control file to write. activated reports whether p's
// version is current, also when err is set: an error after the switch
// leaves the new version current, possibly with the old control file.
func (h *Host) activate(p *placement, confirm decideFunc) (out *Installed, activated bool, err error) {
	unlock, err := h.Dir.Lock()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = unlock() }()
	m := p.v.Manifest
	if err := h.checkPlaceable(m, p.features); err != nil {
		return nil, false, err
	}
	if p.ctl, err = confirm(p.v, p.hash); err != nil {
		return nil, false, err
	}
	if _, err := os.Stat(h.Dir.Version(m.ID, m.Version)); err != nil {
		// Another install of the id made a version current meanwhile and
		// kept only that one and its previous.
		return nil, false, fmt.Errorf("%s %s was removed by another install of %s meanwhile; try again: %w", m.ID, m.Version, m.ID, err)
	}
	return h.activateLocked(p)
}

// activateLocked makes p's version current with control file p.ctl; the
// caller holds the data directory's lock.
func (h *Host) activateLocked(p *placement) (out *Installed, activated bool, err error) {
	m, ctl := p.v.Manifest, p.ctl
	out = &Installed{Verified: p.v, ApprovalHash: p.hash, Enabled: ctl.Enabled, Unchanged: p.unchanged}
	previous, err := h.Dir.Activate(m.ID, m.Version)
	if err != nil {
		current, _ := h.Dir.Current(m.ID)
		return out, current == m.Version, err
	}
	if previous != m.Version {
		out.Previous = previous
	}
	if err := h.Dir.WriteControl(m.ID, ctl); err != nil {
		return out, true, err
	}
	return out, true, nil
}

// abandon removes the version directory p unpacked, unless it is current.
func (h *Host) abandon(p *placement) {
	if !p.unpacked {
		return
	}
	unlock, err := h.Dir.Lock()
	if err != nil {
		return
	}
	defer func() { _ = unlock() }()
	m := p.v.Manifest
	if current, err := h.Dir.Current(m.ID); err == nil && current != m.Version {
		_ = layout.RemoveAll(h.Dir.Version(m.ID, m.Version))
	}
}

// place unpacks a package (unpack, with decide) and makes it current.
// Without claim both happen under one hold of the data directory's lock;
// with it, claim runs between them with no lock held, and activate decides
// again. A package refused after it was unpacked is abandoned.
func (h *Host) place(r io.Reader, features FeatureOwners, decide decideFunc, claim func(id string)) (*Installed, error) {
	if claim == nil {
		unlock, err := h.Dir.Lock()
		if err != nil {
			return nil, err
		}
		defer func() { _ = unlock() }()
		p, err := h.unpackLocked(r, features, decide)
		if err != nil {
			return nil, err
		}
		in, activated, err := h.activateLocked(p)
		if err != nil && !activated && p.unpacked {
			_ = layout.RemoveAll(h.Dir.Version(p.v.Manifest.ID, p.v.Manifest.Version))
		}
		return in, err
	}
	p, err := h.unpack(r, features, decide)
	if err != nil {
		return nil, err
	}
	claim(p.v.Manifest.ID)
	in, activated, err := h.activate(p, decide)
	if err != nil {
		if !activated {
			h.abandon(p)
		}
		return nil, err
	}
	return in, nil
}
