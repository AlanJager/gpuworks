package extpkg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg/archive"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Markers are the persisted version markers; *store.Store satisfies it.
type Markers interface {
	PluginMarkers() ([]extapi.Marker, error)
	DeletePluginMarker(name string) error
}

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
}

// Install reads a package from r, verifies it in an incoming directory,
// moves it to its version directory and makes it current. Nothing is
// unpacked into place unless every check passes. A version already
// unpacked with the same signed manifest is made current again; one with a
// different manifest is refused (ErrorPluginVersionConflict).
func (h *Host) Install(r io.Reader, opt InstallOptions) (*Installed, error) {
	unlock, err := h.Dir.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Under the lock no other install is running: what an interrupted one
	// left behind goes first.
	if err := h.Dir.Cleanup(); err != nil {
		return nil, err
	}
	stage, err := h.Dir.Stage()
	if err != nil {
		return nil, err
	}
	defer stage.Abort()
	c, err := archive.Read(r, stage, h.limits())
	if err != nil {
		return nil, err
	}
	v, err := h.Verifier.Verify(c, Install)
	if err != nil {
		return nil, err
	}
	m := v.Manifest
	if err := h.checkDataMarker(m); err != nil {
		return nil, err
	}
	hash, err := ApprovalHash(v)
	if err != nil {
		return nil, err
	}
	if opt.RequireApproval && opt.Approve != hash {
		return nil, approvalRequired(m, hash)
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
	ctl := layout.Control{Enabled: opt.Enable, ApprovedHash: string(hash), ApprovedBy: opt.ApprovedBy, At: h.now().UTC()}
	if err := h.Dir.WriteControl(m.ID, ctl); err != nil {
		return nil, err
	}
	return out, nil
}

// approvalRequired is the refusal that names what an install asks the
// administrator to approve.
func approvalRequired(m *extapi.Manifest, hash extapi.Digest) error {
	return messages.NewProductError(messages.ErrorPluginApprovalRequired,
		fmt.Sprintf("installing %s %s needs approval %s", m.ID, m.Version, hash),
		map[string]any{"id": m.ID, "version": m.Version, "approve_hash": string(hash),
			"permissions": strings.Join(m.Permissions, ","), "roles": strings.Join(m.Roles, ",")})
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
	defer unlock()
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

// checkDataMarker refuses a build older than the data its plugin already
// wrote: the stored data marker of the package may not be newer than the
// build's storage.state_version (0 for a build without storage).
func (h *Host) checkDataMarker(m *extapi.Manifest) error {
	stored, err := h.Markers.PluginMarkers()
	if err != nil {
		return fmt.Errorf("read plugin version markers: %w", err)
	}
	name := extapi.PluginDataMarker(m.ID)
	version := 0
	if m.Storage != nil {
		version = m.Storage.StateVersion
	}
	var own []extapi.Marker
	for _, s := range stored {
		if s.Name == name {
			own = append(own, s)
		}
	}
	return extapi.CheckMarkers(own, map[string]int{name: version})
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
	defer unlock()
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
			if has, err := h.hasMarker(marker); err != nil || !has {
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

func (h *Host) hasMarker(name string) (bool, error) {
	stored, err := h.Markers.PluginMarkers()
	if err != nil {
		return false, err
	}
	for _, m := range stored {
		if m.Name == name {
			return true, nil
		}
	}
	return false, nil
}
