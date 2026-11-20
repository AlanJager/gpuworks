package extpkg

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg/archive"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Host installs, lists and removes packages in one data directory. It works
// with the server stopped (the command line) as well as inside it.
type Host struct {
	Dir      layout.Dir
	Verifier *Verifier
	Markers  Markers
	// Limits bound a package read; the zero value is archive.DefaultLimits.
	Limits archive.Limits
	// Hasher remembers installed files' hashes between checks; nil hashes
	// every file on every check.
	Hasher *layout.Hasher
	Now    func() time.Time
}

func (h *Host) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Host) limits() archive.Limits {
	if h.Limits == (archive.Limits{}) {
		return archive.DefaultLimits()
	}
	return h.Limits
}

func (h *Host) hasher() *layout.Hasher {
	if h.Hasher != nil {
		return h.Hasher
	}
	return layout.NewHasher()
}

// Installed is the outcome of an install.
type Installed struct {
	*Verified
	// Previous is the version that was active before, kept for rollback.
	Previous string
	// Unchanged reports that this exact build was already unpacked.
	Unchanged bool
	// ApprovalHash is what the installer approved.
	ApprovalHash extapi.Digest
	// Enabled reports whether the package may run now that it is placed.
	Enabled bool
}

// InstallOptions are an install's choices.
type InstallOptions struct {
	// Enable starts the package's server part once a server runs it.
	// Installing over an installed version that is enabled keeps it
	// enabled either way.
	Enable bool
	// ApprovedBy names who approved the package's permissions.
	ApprovedBy string
	// RequireApproval refuses the install unless Approve equals the
	// package's approval hash (ErrorPluginApprovalRequired, carrying the
	// hash and what it covers), before anything is unpacked into place:
	// an administrator approves exactly what the package asks for.
	RequireApproval bool
	Approve         extapi.Digest
	// Features, when set, refuses a package declaring a feature key
	// someone else provides (ErrorPluginFeatureTaken), before approval.
	Features FeatureOwners
	// ReservedIDs are ids no package may take: the names of the modules
	// compiled into the server, whose routes live under the same
	// /api/v1/x/<name>/ (ErrorPluginIDTaken), refused before approval.
	ReservedIDs []string
	// Claim, when set, runs once the package is verified, accepted and
	// unpacked to its version directory, before it is made current, with
	// no lock of the data directory held: a server takes the lock of the
	// package's id there. What was decided is checked again after it.
	Claim func(id string)
}

// Install reads a package from r, verifies it in an incoming directory,
// moves it to its version directory and makes it current. Nothing is
// unpacked into place unless every check passes. A version already
// unpacked with the same signed manifest is made current again; one with a
// different manifest is refused (ErrorPluginVersionConflict).
func (h *Host) Install(r io.Reader, opt InstallOptions) (*Installed, error) {
	return h.place(r, opt.Features, func(v *Verified, hash extapi.Digest) (layout.Control, error) {
		if err := CheckIDFree(v.Manifest.ID, opt.ReservedIDs); err != nil {
			return layout.Control{}, err
		}
		if err := h.checkNotUpgrading(v.Manifest.ID); err != nil {
			return layout.Control{}, err
		}
		current, err := h.Dir.Current(v.Manifest.ID)
		if err != nil {
			return layout.Control{}, err
		}
		enabled := opt.Enable
		if current != "" {
			// Installing over an installed version keeps it enabled: an
			// install without enable never turns a running package off. An
			// unreadable control file does not block the reinstall that
			// repairs it.
			if ctl, _, err := h.Dir.ReadControl(v.Manifest.ID); err == nil {
				enabled = enabled || ctl.Enabled
			}
		}
		if opt.RequireApproval && opt.Approve != hash {
			return layout.Control{}, approvalRequired(v.Manifest, hash, h.diffFromCurrent(v, current))
		}
		return layout.Control{Enabled: enabled, ApprovedHash: string(hash), ApprovedBy: opt.ApprovedBy, At: h.now().UTC()}, nil
	}, opt.Claim)
}

// diffFromCurrent is what v asks for compared with installed version
// current of its id, or with nothing when current is empty. An installed
// version that no longer verifies is diffed as nothing: the administrator
// sees everything the new one asks for.
func (h *Host) diffFromCurrent(v *Verified, current string) ApprovalDiff {
	if current == "" {
		return DiffApproval(nil, v)
	}
	old, _ := h.Recheck(v.Manifest.ID, false)
	d := DiffApproval(old, v)
	d.FromVersion = current
	return d
}

// SetEnabled records whether installed package id may run: a server starts
// an enabled package's server part and stops a disabled one. What the
// package pushed stays enforced either way. The approval stays as it was.
func (h *Host) SetEnabled(id string, enabled bool) error {
	if !extapi.ValidPackageID(id) {
		return notInstalled(id)
	}
	unlock, err := h.Dir.Lock()
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	version, err := h.Dir.Current(id)
	if err != nil {
		return err
	}
	if version == "" {
		return notInstalled(id)
	}
	ctl, _, err := h.Dir.ReadControl(id)
	if err != nil {
		return err
	}
	ctl.Enabled, ctl.At = enabled, h.now().UTC()
	return h.Dir.WriteControl(id, ctl)
}

// Entry is one installed package as List reports it.
type Entry struct {
	ID      string
	Version string
	// Versions are every unpacked version, the current one among them.
	Versions []string
	Control  layout.Control
	// Verified is the package as it verified now; nil when Err is set.
	Verified *Verified
	// Err is why the installed package no longer verifies (a changed file,
	// a capability this server lacks, ...): it is broken and must not run.
	Err error
}

// List verifies every installed package again, hashing changed files only.
func (h *Host) List() ([]Entry, error) {
	ids, err := h.Dir.Installed()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(ids))
	for _, id := range ids {
		e := Entry{ID: id}
		if e.Version, err = h.Dir.Current(id); err != nil {
			return nil, err
		}
		if e.Versions, err = h.Dir.Versions(id); err != nil {
			return nil, err
		}
		if e.Control, _, err = h.Dir.ReadControl(id); err != nil {
			e.Err = err
		}
		v, err := h.Recheck(id, false)
		if err != nil {
			e.Err = err
		} else {
			e.Verified = v
		}
		out = append(out, e)
	}
	return out, nil
}

// Recheck verifies the current version of installed package id again.
// full rehashes every file (at boot); otherwise only files whose size,
// modification time or inode changed since the Host last hashed them are
// read (before each start).
func (h *Host) Recheck(id string, full bool) (*Verified, error) {
	if !extapi.ValidPackageID(id) {
		return nil, notInstalled(id)
	}
	version, err := h.Dir.Current(id)
	if err != nil {
		return nil, err
	}
	if version == "" {
		return nil, notInstalled(id)
	}
	return h.recheckVersion(id, version, full)
}

// recheckVersion verifies unpacked version of package id again, whether
// it is current or not.
func (h *Host) recheckVersion(id, version string, full bool) (*Verified, error) {
	c, err := layout.ReadInstalled(h.Dir.Version(id, version), h.hasher(), full)
	if err != nil {
		return nil, err
	}
	v, err := h.Verifier.Verify(c, Recheck)
	if err != nil {
		return nil, err
	}
	if v.Manifest.ID != id || v.Manifest.Version != version {
		return nil, messages.NewProductError(messages.ErrorPluginManifestMismatch,
			fmt.Sprintf("installed %s %s holds the manifest of %s %s", id, version, v.Manifest.ID, v.Manifest.Version),
			map[string]any{})
	}
	return v, nil
}

func notInstalled(id string) error {
	return messages.NewProductError(messages.ErrorPluginNotInstalled, fmt.Sprintf("no plugin %s is installed", id), map[string]any{"id": id})
}

// Remove uninstalls package id. Without purge the plugin's data stays
// (and so does its data marker); with purge the package directory moves to
// the trash and the data marker is deleted. The floor of the package stays
// either way, so an older build cannot come back by reinstalling.
func (h *Host) Remove(id string, purge bool) error {
	if !extapi.ValidPackageID(id) {
		return notInstalled(id)
	}
	unlock, err := h.Dir.Lock()
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	version, err := h.Dir.Current(id)
	if err != nil {
		return err
	}
	marker := extapi.PluginDataMarker(id)
	if version == "" {
		_, statErr := os.Stat(h.Dir.Package(id))
		switch {
		case !purge:
			return notInstalled(id)
		case statErr != nil:
			// Finish a purge whose marker delete failed after the move.
			if _, has, err := h.dataMarker(id); err != nil || !has {
				return errors.Join(err, notInstalled(id))
			}
			return h.Markers.DeletePluginMarker(marker)
		}
	}
	if err := h.Dir.Remove(id, purge, h.now()); err != nil {
		return err
	}
	if purge {
		return h.Markers.DeletePluginMarker(marker)
	}
	return nil
}
