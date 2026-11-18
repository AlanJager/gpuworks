package extpkg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
}

// InstallOptions are an install's choices.
type InstallOptions struct {
	// Enable starts the package's server part once a server runs it.
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
}

// Install reads a package from r, verifies it in an incoming directory,
// moves it to its version directory and makes it current. Nothing is
// unpacked into place unless every check passes. A version already
// unpacked with the same signed manifest is made current again; one with a
// different manifest is refused (ErrorPluginVersionConflict).
func (h *Host) Install(r io.Reader, opt InstallOptions) (*Installed, error) {
	return h.place(r, opt.Features, func(v *Verified, hash extapi.Digest) (layout.Control, error) {
		if opt.RequireApproval && opt.Approve != hash {
			return layout.Control{}, approvalRequired(v.Manifest, hash, DiffApproval(nil, v))
		}
		return layout.Control{Enabled: opt.Enable, ApprovedHash: string(hash), ApprovedBy: opt.ApprovedBy, At: h.now().UTC()}, nil
	})
}

// UpgradeOptions are an upgrade's choices.
type UpgradeOptions struct {
	// ApprovedBy names who approved the upgrade.
	ApprovedBy string
	// Approve must equal the incoming package's approval hash, unless the
	// hash is the approved one and the site policy auto-approves that.
	Approve extapi.Digest
	// Features, when set, refuses a package declaring a feature key
	// someone else provides (ErrorPluginFeatureTaken), before approval.
	Features FeatureOwners
}

// Upgraded is the outcome of an upgrade.
type Upgraded struct {
	*Installed
	// Diff is what the new version asks for compared with the old one.
	Diff ApprovalDiff
	// AutoApproved reports that the approval hash was unchanged and the
	// site policy applied the upgrade without a new approval.
	AutoApproved bool
}

// Upgrade replaces installed package id with the package read from r, the
// way Install places it, keeping whether the package is enabled. The
// package's id must be installed (ErrorPluginNotInstalled). Without
// approval it is refused with ErrorPluginApprovalRequired, whose params
// carry the diff from the installed version (an *ApprovalNeeded cause
// holds it too), and nothing changes. An approval hash equal to the
// approved one counts as approved when the site policy auto-approves
// unchanged upgrades.
func (h *Host) Upgrade(r io.Reader, opt UpgradeOptions) (*Upgraded, error) {
	up := &Upgraded{}
	in, err := h.place(r, opt.Features, func(v *Verified, hash extapi.Digest) (layout.Control, error) {
		m := v.Manifest
		current, err := h.Dir.Current(m.ID)
		if err != nil {
			return layout.Control{}, err
		}
		if current == "" {
			return layout.Control{}, notInstalled(m.ID)
		}
		ctl, _, err := h.Dir.ReadControl(m.ID)
		if err != nil {
			return layout.Control{}, err
		}
		// An installed version that no longer verifies is diffed as
		// nothing: the administrator sees everything the new one asks for.
		old, _ := h.Recheck(m.ID, false)
		up.Diff = DiffApproval(old, v)
		up.Diff.FromVersion = current
		up.Diff.HashChanged = string(hash) != ctl.ApprovedHash
		up.AutoApproved = !up.Diff.HashChanged && h.Verifier.Policy.AutoApproveUnchanged
		approvedBy := opt.ApprovedBy
		switch {
		case up.AutoApproved:
			// Nothing new was approved: the earlier approver stands.
			approvedBy = ctl.ApprovedBy
		case opt.Approve != hash:
			return layout.Control{}, approvalRequired(m, hash, up.Diff)
		}
		return layout.Control{
			Enabled: ctl.Enabled, ApprovedHash: string(hash), ApprovedBy: approvedBy, At: h.now().UTC(),
			// Host setup steps already applied stay applied unless the
			// new version asks for one more.
			HostSetupApplied: ctl.HostSetupApplied && len(up.Diff.HostSetupAdded) == 0,
		}, nil
	})
	if err != nil {
		return nil, err
	}
	up.Installed = in
	return up, nil
}

// place reads, verifies and unpacks a package under the data directory's
// lock. decide runs after verification, before anything is moved into
// place: it refuses the package or returns the control file to write.
func (h *Host) place(r io.Reader, features FeatureOwners, decide func(*Verified, extapi.Digest) (layout.Control, error)) (*Installed, error) {
	unlock, err := h.Dir.Lock()
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock() }()
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
	if err := h.CheckDataMarker(m); err != nil {
		return nil, err
	}
	if err := checkFeatureOwners(m, features); err != nil {
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
	out := &Installed{Verified: v, ApprovalHash: hash}
	switch existing, err := os.ReadFile(filepath.Join(h.Dir.Version(m.ID, m.Version), archive.ManifestMember)); {
	case err == nil:
		if !bytes.Equal(existing, c.Manifest) {
			return nil, messages.NewProductError(messages.ErrorPluginVersionConflict,
				fmt.Sprintf("%s %s is installed with other contents", m.ID, m.Version),
				map[string]any{"id": m.ID, "version": m.Version})
		}
		out.Unchanged = true
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
	default:
		return nil, err
	}
	if out.Previous, err = h.Dir.Activate(m.ID, m.Version); err != nil {
		return nil, err
	}
	if out.Previous == m.Version {
		out.Previous = ""
	}
	if err := h.Dir.WriteControl(m.ID, ctl); err != nil {
		return nil, err
	}
	return out, nil
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
